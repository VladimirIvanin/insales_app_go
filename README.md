# InSales App Go

[![CI](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/ci.yml/badge.svg)](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/ci.yml)
[![Security Audit](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/security.yml/badge.svg)](https://github.com/ivaninvladimir/insales_app_go/actions/workflows/security.yml)

Устанавливаемое партнёрское приложение для платформы [InSales](https://www.insales.ru) на Go.

Первая версия реализует:
- установка, переустановка и удаление приложения;
- вход сотрудника магазина через InSales (token3);
- поддержка нескольких магазинов с изоляцией данных;
- минимальный HTTP-клиент InSales API (GET /admin/account.json);
- постоянное хранилище подключений и серверных сессий (SQLite);
- шифрование паролей магазинов (AES-256-GCM);
- автоматические тесты и GitHub Actions (CI + security audit);
- фоновый Lifecycle с обработкой конкурентных установок.

## Быстрый старт

### Требования

- Go 1.23+
- SQLite (pure-Go через `modernc.org/sqlite`, CGO не требуется)

### Установка и запуск

```bash
# Клонировать репозиторий
git clone https://github.com/ivaninvladimir/insales_app_go.git
cd insales_app_go

# Скопировать и настроить переменные окружения
cp .env.example .env
# Отредактировать .env со своими значениями

# Запустить тесты
go test -race -coverprofile=coverage.out ./...

# Собрать
go build -o bin/server ./cmd/server

# Запустить
./bin/server
```

### Переменные окружения

| Переменная | Обязательная | Описание |
|---|---|---|
| `INSALES_APP_ID` | Да | Публичный идентификатор приложения |
| `INSALES_APP_SECRET` | Да | Секретный ключ приложения |
| `APP_BASE_URL` | Да | Публичный URL приложения (для callback) |
| `ALLOWED_SHOPS` | Нет | Список разрешённых доменов магазинов через запятую |
| `APP_DB` | Нет | Путь к файлу SQLite (по умолчанию `runtime/app.sqlite3`) |
| `BIND_HOST` | Нет | Адрес для привязки сервера (по умолчанию `127.0.0.1`) |
| `PORT` | Нет | Порт сервера (по умолчанию `8765`) |
| `ENCRYPTION_KEY` | Да | 32 байта hex для AES-256: `openssl rand -hex 32` |
| `LOG_LEVEL` | Нет | Уровень логирования: debug, info, warn, error |

### Генерация ключа шифрования

```bash
openssl rand -hex 32
```

## Создание приложения в InSales

1. В панели управления InSales перейдите в **Приложения → Партнёрские приложения → Создать приложение**.
2. Укажите:
   - **Название**: любое
   - **URL установки**: `https://your-domain.com/install`
   - **URL входа**: `https://your-domain.com/login`
   - **URL удаления**: `https://your-domain.com/uninstall`
   - **Callback URL**: `https://your-domain.com/auth/callback`
3. После создания приложения запишите **App ID** и **App Secret**.
4. Минимальные права: достаточно права на чтение (`read`) для проверки `/admin/account.json`.

## Маршруты

| Маршрут | Метод | Назначение |
|---|---|---|
| `GET /install` | GET | Приём установки от InSales |
| `GET /login` | GET | Начало входа через InSales |
| `GET /auth/callback` | GET | Проверка ответа InSales (token3) |
| `GET /` | GET | Страница приложения (требуется сессия) |
| `POST /probe` | POST | Проверочный запрос к API магазина |
| `POST /logout` | POST | Завершение сессии |
| `GET /uninstall` | GET | Удаление подключения |
| `GET /healthz` | GET | Проверка работоспособности |

## Архитектура

```
cmd/server/           # Точка входа, DI-фабрика, graceful shutdown
internal/
  config/             # Загрузка конфигурации из переменных окружения
  insales/
    protocol.go       # Формулы MD5 (password, token, token2, token3)
    client.go         # HTTP-клиент InSales API (GetAccount)
  storage/            # SQLite-хранилище с миграциями
  lifecycle/          # Сценарии install, login, autologin, uninstall
  httpapp/            # HTTP-сервер, маршруты, middleware, шаблоны
  testutil/           # Симулятор платформы для тестов
templates/            # HTML-шаблоны (dashboard, error, install_ok)
migrations/           # Версионированные SQL-миграции (embedded)
.github/workflows/    # CI и Security workflow
```

## Тестирование

### Автономные тесты (без внешних зависимостей)

```bash
go test -race -coverprofile=coverage.out ./...
```

Тесты используют:
- **test vectors**: фиксированные эталонные значения для password и token3 (cross-language);
- **httptest**: локальный HTTP-сервер для API-клиента;
- **временные SQLite**: изолированные БД на каждый тест;
- **симулятор платформы**: эмуляция InSales install/login/uninstall flow.

### Ручная проверка на тестовом магазине

1. Настройте `.env` с реальными параметрами приложения.
2. Обеспечьте HTTPS-доступ к приложению (ngrok, reverse proxy).
3. Установите приложение в тестовый магазин через панель InSales.
4. Проверьте сценарий: установка → вход → проверка API → удаление.

Успех симулятора **не** подтверждает совместимость с реальной платформой.

## Проверенные сценарии

- Установка с верификацией через API
- Отклонение установки при несовпадении account.id
- Отклонение установки для незарегистрированного магазина
- Переустановка с увеличением поколения
- Установка → вход → проверка API → удаление (полный lifecycle)
- Удаление с неверным паролем (отклоняется)
- Удаление с верным паролем (удаляет магазин, сессии, challenges)
- Идемпотентность повторного удаления
- Истечение срока сессии
- Изоляция сессий разных магазинов
- Consumption of one-time challenge
- Привязка challenge к браузеру

## Ограничения первой версии

- **Один процесс**: всё приложение работает в одном процессе. Для горизонтального масштабирования требуется отдельная очередь заданий.
- **SQLite**: хранение в одном SQLite-файле. Для production-нагрузок рекомендуется PostgreSQL или другая СУБД.
- **Очередь**: фоновые проверки установки выполняются через внутренний канал и горутину. Нет внешней очереди (Redis, RabbitMQ).
- **Вебхуки, подписки, OAuth/OIDC покупателей, SPA** не реализованы.

## Расширение

### Добавление API-метода

```go
// internal/insales/client.go
func (c *Client) GetOrders(ctx context.Context) ([]Order, error) {
    // ...
}
```

### Добавление страницы

1. Создайте шаблон в `templates/`.
2. Добавьте маршрут в `internal/httpapp/httpapp.go`.

## Зависимости

- Стандартная библиотека Go (net/http, html/template, database/sql, crypto/*, log/slog)
- `modernc.org/sqlite` — pure-Go SQLite (без CGO)

## Ревизии использованных примеров

- [Оригинальное приложение InSales на Ruby](https://github.com/insales/insales_app) — revision `78593a409e8e838c2567c5645c4354e36f879a24`
- [Заготовка на Python](https://github.com/VladimirIvanin/insales_app_python) — revision изучена для протокола
- [Заготовка на PHP](https://github.com/VladimirIvanin/insales_app_php) — revision изучена для архитектуры
- [Python-demo в поставке skill](.gigacode/skills/insales-partner-app-expert/assets/python-demo/) — протокол и тестовые сценарии

## Лицензия

MIT