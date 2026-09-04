package spam

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/hickeroar/gobayes/v3/bayes"
	"golang.org/x/text/unicode/norm"
)

const (
	categorySpam      = "spam"
	categoryHam       = "ham"
	learnedTokenFloor = 3
)

var (
	linkPattern    = regexp.MustCompile(`(?i)(https?://|www\.|t\.me/|telegram\.me/|wa\.me/)`)
	contactPattern = regexp.MustCompile(`(?i)(писать|пиши(?:те)?|обращай(?:тесь)?)[^\n]{0,24}(лс|личк|директ)|@[a-z0-9_]{4,}`)
	moneyPattern   = regexp.MustCompile(`(?i)(\d[\d ]{2,}\s*(₽|руб|р\.|\$|доллар|usd)|от\s+\d[\d ]{2,})`)
)

// Result contains the classifier score and independent signals that made an
// automatic action eligible. Score is a normalized spam-vs-ham ratio.
type Result struct {
	Spam    bool
	Score   float64
	Signals []string
}

// Detector combines a Russian-trained Bayesian classifier with conservative
// structural signals. Bayesian confidence alone never triggers a bot vote.
type Detector struct {
	classifier *bayes.Classifier
	threshold  float64
	mu         sync.Mutex
	learned    map[string]struct{}
	tokenCount map[string]int
}

func NewDetector(threshold float64) (*Detector, error) {
	if threshold < 0.5 || threshold > 1 {
		return nil, fmt.Errorf("spam threshold must be between 0.5 and 1")
	}

	classifier := bayes.NewClassifierWithOptions("russian", false)
	for _, sample := range spamSamples {
		if err := classifier.Train(categorySpam, normalize(sample)); err != nil {
			return nil, fmt.Errorf("train spam sample: %w", err)
		}
	}
	for _, sample := range hamSamples {
		if err := classifier.Train(categoryHam, normalize(sample)); err != nil {
			return nil, fmt.Errorf("train ham sample: %w", err)
		}
	}

	return &Detector{
		classifier: classifier,
		threshold:  threshold,
		learned:    make(map[string]struct{}),
		tokenCount: make(map[string]int),
	}, nil
}

func (d *Detector) Detect(text string) Result {
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}
	}

	normalized := normalize(text)
	d.mu.Lock()
	scores := d.classifier.Score(normalized)
	learnedSignal := d.hasLearnedToken(normalized)
	d.mu.Unlock()
	spamScore, hamScore := scores[categorySpam], scores[categoryHam]
	var score float64
	if total := spamScore + hamScore; total > 0 {
		score = spamScore / total
	}

	signals := detectSignals(text, normalized)
	if learnedSignal {
		signals = append(signals, "learned")
	}
	return Result{
		Spam:    score >= d.threshold && len(signals) >= 2,
		Score:   score,
		Signals: signals,
	}
}

func (d *Detector) LearnSpam(chatID int64, messageID int, text string) error {
	normalized := normalize(strings.TrimSpace(text))
	if normalized == "" {
		return nil
	}
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.learned[key]; ok {
		return nil
	}
	if err := d.classifier.Train(categorySpam, normalized); err != nil {
		return err
	}
	for token := range significantTokens(normalized) {
		d.tokenCount[token]++
	}
	d.learned[key] = struct{}{}
	return nil
}

func (d *Detector) hasLearnedToken(text string) bool {
	for token := range significantTokens(text) {
		if d.tokenCount[token] >= learnedTokenFloor {
			return true
		}
	}
	return false
}

func significantTokens(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(token)) < 5 || commonTokens[token] {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

var commonTokens = map[string]bool{
	"будет": true, "вашего": true, "всего": true, "когда": true,
	"который": true, "можно": true, "нужно": true, "очень": true,
	"после": true, "почему": true, "привет": true, "сегодня": true,
	"сообщение": true, "также": true, "только": true, "чтобы": true,
}

func detectSignals(original, normalized string) []string {
	var signals []string
	if linkPattern.MatchString(original) {
		signals = append(signals, "link")
	}
	if contactPattern.MatchString(original) || contactPattern.MatchString(normalized) {
		signals = append(signals, "contact")
	}
	if moneyPattern.MatchString(original) || moneyPattern.MatchString(normalized) {
		signals = append(signals, "money")
	}
	if containsAny(normalized,
		"заработ", "подработ", "доход", "прибыл", "вложен", "инвест",
		"крипт", "казино", "ставк", "выплат", "ваканси", "удаленн",
	) {
		signals = append(signals, "offer")
	}
	if containsAny(normalized,
		"без влож", "гарантир", "ежедневн", "в день", "мест огранич",
		"легкие деньги", "быстрые деньги",
	) {
		signals = append(signals, "promise")
	}
	if containsAny(normalized, "vpn", "впн", "proxy", "прокси") {
		signals = append(signals, "vpn")
	}
	if containsAny(normalized, "бесплатн", "free", "даром") {
		signals = append(signals, "free")
	}
	if countAny(normalized, "telegram", "tiktok", "youtube", "instagram", "whatsapp") >= 2 {
		signals = append(signals, "services")
	}
	return signals
}

func countAny(text string, fragments ...string) int {
	count := 0
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			count++
		}
	}
	return count
}

func containsAny(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func normalize(text string) string {
	text = strings.ToLower(norm.NFKC.String(text))
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff':
			return -1
		default:
			return r
		}
	}, text)
	text = collapseRepeats(text, 2)
	return compactSpacedLetters(text)
}

func collapseRepeats(text string, limit int) string {
	var result strings.Builder
	var previous rune
	count := 0
	for _, current := range text {
		if current == previous {
			count++
		} else {
			previous, count = current, 1
		}
		if count <= limit || !unicode.IsLetter(current) {
			result.WriteRune(current)
		}
	}
	return result.String()
}

func compactSpacedLetters(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return text
	}

	var result []string
	for i := 0; i < len(fields); {
		j := i
		for j < len(fields) && len([]rune(fields[j])) == 1 {
			j++
		}
		if j-i >= 3 {
			result = append(result, strings.Join(fields[i:j], ""))
			i = j
			continue
		}
		result = append(result, fields[i])
		i++
	}
	return strings.Join(result, " ")
}

var spamSamples = []string{
	"Предлагаю удаленную подработку, высокий доход каждый день, пишите в личку",
	"Заработок без вложений от 5000 рублей в день, подробности в ЛС",
	"Набираем людей на удаленную работу, выплаты ежедневно, пиши мне",
	"Инвестиции с гарантированной прибылью, переходите по ссылке",
	"Раскрутка депозита и быстрый заработок на криптовалюте",
	"Есть легкая подработка со свободным графиком, пишите в директ",
	"Получай доход не выходя из дома, места ограничены",
	"Онлайн заработок без опыта, оплата сразу после задания",
	"Увеличу ваш капитал в несколько раз без риска",
	"Срочная вакансия, удаленная занятость два часа в день",
	"Ставки на спорт с гарантированным проходом, подпишись на канал",
	"Лучшее онлайн казино, большой бонус после регистрации",
	"Переведу деньги за простые действия, обращайтесь в личные сообщения",
	"Ищу партнеров в прибыльный проект, вложения окупятся за день",
	"Крипта приносит стабильный пассивный доход, расскажу в ЛС",
	"Всем по 10000 рублей, забирайте выплату по ссылке",
	"Удаленная вакансия без собеседования, начать можно сегодня",
	"Работа для всех желающих, ежедневные выплаты на карту",
	"Быстрые деньги без документов и проверок",
	"Заработай прямо сейчас, регистрируйся по моей ссылке",
	"VPN с которым летает Telegram TikTok YouTube бесплатно",
	"Бесплатный VPN для Telegram и YouTube, подключайтесь",
}

var hamSamples = []string{
	"Всем привет, встречаемся завтра в семь часов",
	"Подскажите пожалуйста, когда состоится следующая встреча",
	"Спасибо за помощь, проблема уже решена",
	"Кто сегодня сможет проверить новую версию приложения",
	"Прикрепляю ссылку на документацию проекта",
	"Давайте обсудим задачу после обеда",
	"Я немного опоздаю, начинайте без меня",
	"Поздравляю всех с праздником",
	"Кто забрал посылку из пункта выдачи",
	"Сегодня работаю из дома, буду на связи",
	"Оплатил общий счет, переведите свою часть на карту",
	"Ищу хорошего мастера по ремонту, посоветуйте контакты",
	"Выложил фотографии в общий альбом",
	"Напомните адрес нашего офиса",
	"Созвон перенесли на пятницу",
	"Проверьте изменения в репозитории",
	"Продам старый стол, если кому-нибудь нужен",
	"Купил билеты, отправил каждому в личные сообщения",
	"Какие планы на выходные",
	"Добро пожаловать в наш общий чат",
	"Как настроить VPN на роутере для домашней сети",
}
