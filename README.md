# InSales App Go

[![CI](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/ci.yml/badge.svg)](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/ci.yml)
[![Security Audit](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/security.yml/badge.svg)](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/security.yml)

Заготовка устанавливаемого партнёрского приложения для платформы [InSales](https://www.insales.ru) на Go.

Один законченный путь: сохранить установку, открыть приложение через token3, вызвать API магазина и удалить подключение. Пароли магазинов шифруются AES-256-GCM.

Проверка подключения выполняется при первом открытии, а не сразу после install. Фоновых worker и очередей нет.

## Требования

- Go 1.27.3+
- SQLite через `modernc.org/sqlite` (CGO не нужен)

## Запуск

```bash
git clone https://github.com/ivaninvladimir/insales_app_go.git
cd insales_app_go
cp .env.example .env
go build -o bin/server ./cmd/server
./bin/server db upgrade
./bin/server
```

Запуск без аргументов эквивалентен `./bin/server serve`. Уже экспортированные переменные окружения важнее значений из `.env`.

```bash
./bin/server db status
./bin/server doctor
```

`doctor` проверяет настройки, схему и API уже активных подключений. Он не активирует непроверенные установки.

## Регистрация приложения

1. В InSales откройте **Приложения → Партнёрские приложения → Создать приложение**.
2. Укажите:
   - URL установки: `https://your-domain.com/install`
   - URL входа: `https://your-domain.com/login`
   - URL удаления: `https://your-domain.com/uninstall`
3. Запишите App ID и App Secret в `.env`.
4. Добавьте домен магазина в `ALLOWED_SHOPS`. Совпадение точное, после нормализации регистра.
5. Для проверки установки достаточно права на чтение аккаунта. Заказы и товары в примерах клиента требуют соответствующих прав.

Callback входа приложение формирует само при переходе в магазин: `APP_BASE_URL/auth/callback`.

## Жизненный цикл

### Install

`GET /install` проверяет параметры и allowlist, вычисляет пароль `MD5(token + app_secret)` и сохраняет кандидата. Ответ `200` не означает, что подключение уже проверено: запросов к API на этом шаге нет.

Повтор того же пароля не создаёт вторую запись и не сбрасывает сессии. На один домен хранится не больше пяти кандидатов, каждый не дольше семи дней.

### Первый вход

`GET /login` выбирает самый новый кандидат, который новее активного подключения, и делает один запрос `account.json` не дольше пяти секунд.

- Совпавший `account.id` активирует именно этот кандидат, затем начинается вход через token3.
- Таймаут, обрыв сети, `429` и `5xx` оставляют кандидата. Если активного подключения нет, нужно повторить вход. Если оно есть, вход продолжается по нему.
- `401`, `403`, другая окончательная ошибка API и чужой `account.id` удаляют выбранного кандидата.
- Неразборчивый ответ не удаляет кандидата: это не доказательство неверного пароля.

До проверки token3 сессия не создаётся.

### Повторный вход

Если нового кандидата нет, повторный вход не проверяет установку через API. Одноразовый challenge привязан к браузеру и поколению подключения. InSales возвращает token3, приложение открывает серверную сессию.

Pending и active переживают перезапуск процесса.

### Uninstall

`GET /uninstall` в одной транзакции сверяет `insales_id`, домен и пароль подключения.

- Совпал active: удаляются это подключение, его сессии, challenge и pending с тем же паролем.
- Совпал только pending: удаляются эти кандидаты, active с другим паролем остаётся.
- Уже удалённое подключение подтверждается повторно. Неверный пароль существующего подключения отклоняется. Ошибка базы не превращается в успешный ответ.

## API-примеры

Один клиент обслуживает проверку установки, маршруты приложения и тесты. Редиректы не следуют, размер ответа ограничен, ошибки сети, HTTP и формата различаются.

```go
account, err := client.GetAccount(ctx)
orders, err := client.ListOrders(ctx, 1, 50)
order, err := client.GetOrder(ctx, 123)
count, err := client.CountOrders(ctx)
updated, err := client.UpdateOrder(ctx, 123, map[string]any{"status": "cancelled"})

product, err := client.GetProduct(ctx, 456)
products, err := client.ListProducts(ctx, 1, 50)
n, err := client.CountProducts(ctx)
renamed, err := client.UpdateProduct(ctx, 456, map[string]any{"title": "Cup"})
usage := client.UsageLimit()
```

Автоматических повторов запросов нет. `UsageLimit` показывает `API-Usage-Limit` последнего ответа этого клиента.

## Маршруты

| Метод | Путь | Назначение |
|---|---|---|
| GET | `/install` | Принять установку |
| GET | `/login` | Проверить кандидата и начать token3 |
| GET | `/auth/callback` | Проверить token3 и открыть сессию |
| GET | `/` | Страница приложения |
| POST | `/probe` | Проверочный запрос `account.json` |
| POST | `/logout` | Завершить сессию |
| GET | `/uninstall` | Отменить подключение |
| GET | `/healthz` | Проверка процесса |

## Настройки

| Переменная | Обязательная | Описание |
|---|---|---|
| `INSALES_APP_ID` | Да | Идентификатор приложения |
| `INSALES_APP_SECRET` | Да | Секрет приложения |
| `APP_BASE_URL` | Да | Публичный URL приложения |
| `ALLOWED_SHOPS` | Да | Домены магазинов через запятую, точное совпадение |
| `ENCRYPTION_KEY` | Да | 32 байта hex: `openssl rand -hex 32` |
| `APP_DB` | Нет | Файл SQLite, по умолчанию `runtime/app.sqlite3` |
| `BIND_HOST` | Нет | По умолчанию `127.0.0.1` |
| `PORT` | Нет | По умолчанию `8765` |
| `LOG_LEVEL` | Нет | `debug`, `info`, `warn`, `error` |

Credentials и query-параметры авторизации в логи не пишутся.

## Тесты

```bash
go test -race -coverprofile=coverage.out ./...
go vet ./...
go build ./...
```

Автономные тесты используют временную SQLite и симулятор платформы. Обновление заказа и товара проверяется на симуляторе и не изменяет магазин.

Отдельная проверка на тестовом магазине: install, первый вход, probe, перезапуск, повторный вход, uninstall, повторная установка. Успех симулятора не доказывает совместимость с платформой.

## Ограничения

- Один web-процесс и один файл SQLite.
- Фоновых worker и очередей нет. Без активного подключения временная ошибка проверки требует нового входа.
- Непроверенный кандидат не даёт сессию и не заменяет действующее подключение.
- Webhooks, подписки, OAuth/OIDC покупателей и многопроцессный запуск не входят в заготовку.

## Структура

```
cmd/server/                 # serve, db upgrade, db status, doctor
internal/config/            # окружение и .env
internal/insales/           # протокол, транспорт, account, orders, products
internal/storage/           # SQLite и миграции
internal/lifecycle/         # install, login, uninstall
internal/httpapp/templates/ # единственный каталог шаблонов
internal/testutil/          # симулятор платформы
```

## Лицензия

MIT
