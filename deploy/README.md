# Развёртывание на VPS

GitHub Actions собирает статический Linux-бинарник, проверяет его SHA-256, заменяет предыдущую версию и перезапускает systemd. SQLite и Telegram-токен при деплое не перезаписываются.

Для деплоя и запуска используются разные непривилегированные пользователи:

- `YOUR_USER` — SSH-пользователь, от имени которого GitHub Actions загружает релизы;
- `ban-dislike-bot` — системный пользователь без shell и SSH-доступа, от имени которого systemd запускает бота.

## 1. Подготовка сервера

Примеры ниже предполагают Ubuntu/Debian и существующего непривилегированного пользователя `YOUR_USER`, под которым GitHub Actions подключается по SSH.

```bash
sudo apt update
sudo apt install -y ca-certificates openssh-server
sudo useradd --system --user-group --home-dir /nonexistent --shell /usr/sbin/nologin ban-dislike-bot
sudo install -d -o YOUR_USER -g YOUR_USER -m 0755 /opt/ban-dislike-bot
sudo install -d -o ban-dislike-bot -g ban-dislike-bot -m 0700 /opt/ban-dislike-bot/data
```

Если пользователь `ban-dislike-bot` уже существует, команда `useradd` завершится с сообщением об ошибке — повторно создавать его не нужно. При переносе существующей базы выдайте системному пользователю права на весь каталог данных:

```bash
sudo chown -R ban-dislike-bot:ban-dislike-bot /opt/ban-dislike-bot/data
sudo chmod 700 /opt/ban-dislike-bot/data
```

Не добавляйте `ban-dislike-bot` в группу SSH/deploy-пользователя: процессу нужны запись только в `data/` и чтение исполняемого файла.

Узнайте архитектуру VPS:

```bash
uname -m
```

Для `x86_64` используйте GitHub Variable `VPS_GOARCH=amd64`, для `aarch64` — `VPS_GOARCH=arm64`.

## 2. Настройка приложения

С локальной машины загрузите шаблон env и systemd unit на сервер:

```bash
scp deploy/ban-dislike-bot.env.example deploy/ban-dislike-bot.service YOUR_USER@YOUR_HOST:/tmp/
```

Затем на сервере создайте секретный env-файл:

```bash
sudo cp /tmp/ban-dislike-bot.env.example /etc/ban-dislike-bot.env
sudo editor /etc/ban-dislike-bot.env
sudo chown root:root /etc/ban-dislike-bot.env
sudo chmod 600 /etc/ban-dislike-bot.env
```

Файл должен содержать настоящий `TELEGRAM_BOT_TOKEN`. Значение `DB_PATH` оставьте `/opt/ban-dislike-bot/data/bot.db`.

## 3. Установка systemd unit

Установите загруженный unit-файл. В нём уже указан отдельный системный пользователь `ban-dislike-bot`:

```bash
sudo cp /tmp/ban-dislike-bot.service /etc/systemd/system/ban-dislike-bot.service
sudo systemctl daemon-reload
sudo systemctl enable ban-dislike-bot
```

Первый запуск произойдёт после первого успешного деплоя. Для ручной проверки:

```bash
sudo systemctl restart ban-dislike-bot
systemctl status ban-dislike-bot
journalctl -u ban-dislike-bot -f
```

## 4. Разрешение на рестарт без пароля

Узнайте полный путь к `systemctl`:

```bash
command -v systemctl
```

Выполните `sudo visudo` и добавьте правило, заменив пользователя и путь при необходимости:

```text
YOUR_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart ban-dislike-bot
```

Не выдавайте deploy-пользователю неограниченный `NOPASSWD` для любых команд systemctl.

## 5. GitHub Environment

Создайте Environment с именем `production`. Добавьте в него secrets:

- `VPS_HOST` — имя или IP сервера;
- `VPS_USER` — SSH-пользователь;
- `VPS_SSH_KEY` — закрытый ключ этого пользователя;
- `VPS_HOST_KEY` — полная проверенная строка для SSH `known_hosts`.

Добавьте Environment variable:

- `VPS_GOARCH` — `amd64` или `arm64`; если не задана, используется `amd64`.

Получите открытый host key через доверенную консоль VPS и сравните его fingerprint с данными провайдера:

```bash
sudo cat /etc/ssh/ssh_host_ed25519_key.pub
sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Значение `VPS_HOST_KEY` должно выглядеть так:

```text
example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
```

Если workflow подключается по IP, в начале строки должен быть тот же IP. Не формируйте secret слепым `ssh-keyscan` без независимой проверки fingerprint.

## 6. Первый деплой

Push в `main` или ручной запуск workflow выполняет:

1. проверку зависимостей, форматирования, линтеров и тестов с race detector;
2. сборку Linux-бинарника;
3. загрузку `.new`-версии и checksum;
4. сохранение текущего бинарника как `.previous`;
5. рестарт и проверку systemd;
6. автоматический откат бинарника, если сервис не поднялся.

Проверить результат на VPS:

```bash
systemctl status ban-dislike-bot
journalctl -u ban-dislike-bot --since '10 minutes ago'
```
