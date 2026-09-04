package spam

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ChannelResult describes a suspicious keyword found in a channel title or
// username. Only channel identity is inspected; message text is irrelevant.
type ChannelResult struct {
	Spam    bool
	Keyword string
}

func DetectChannel(title, username string) ChannelResult {
	for _, value := range []string{title, username} {
		compact := compactChannelName(value)
		for _, keyword := range []string{"vpn", "впн", "proxy", "прокси"} {
			if strings.Contains(compact, keyword) {
				return ChannelResult{Spam: true, Keyword: keyword}
			}
		}

		folded := foldChannelHomoglyphs(compact)
		for _, keyword := range []string{"vpn", "proxy"} {
			if strings.Contains(folded, keyword) {
				return ChannelResult{Spam: true, Keyword: keyword}
			}
		}
	}
	return ChannelResult{}
}

func compactChannelName(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, value)
}

func foldChannelHomoglyphs(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case 'а':
			return 'a'
		case 'с':
			return 'c'
		case 'е':
			return 'e'
		case 'о':
			return 'o'
		case 'р':
			return 'p'
		case 'х':
			return 'x'
		case 'у':
			return 'y'
		default:
			return r
		}
	}, value)
}
