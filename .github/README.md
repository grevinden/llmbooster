# llmbooster

Bifrost-плагин для улучшения запросов перед отправкой в LLM. Использует тот же провайдер и prompt caching для быстрой работы.

## Архитектура

```
Клиент (Copilot SSE)
  → Bifrost
    → mcpfilter PreLLMHook
      ├─ Улучшение через LLM (тот же провайдер, кэш тёплый)
      │    ├─ Контекст отменён → отмена мгновенно, 0 токенов
      │    ├─ Успел → заменяет сообщение на улучшенное
      │    └─ Не успел / ошибка → оригинальный запрос без изменений
      └─ → основной LLM-вызов
```

Запрос к улучшению идёт напрямую к провайдеру (не через Bifrost) через `http.NewRequestWithContext(ctx, ...)`. При отключении клиента `BifrostContext` отменяется, HTTP-запрос прерывается мгновенно.

## Быстрый старт

```bash
make deps     # скачать зависимости
make plugin   # собрать .so
# Установить через Bifrost Web UI → Plugins → Add Plugin → путь к mcpfilter.so
```

## Конфигурация

```json
{
  "enabled": true,
  "system_prompt": "Перефразируй запрос пользователя так, чтобы он был максимально понятным и точным для LLM. Сохрани намерение. Отвечай только улучшенным запросом без пояснений.",
  "model_override": "gpt-4o-mini",
  "timeout_seconds": 10,
  "provider_base_url": "https://api.openai.com/v1",
  "provider_api_key": "env.OPENAI_API_KEY"
}
```

| Поле | Тип | По умолчанию | Описание |
|------|-----|-------------|----------|
| `enabled` | bool | `false` | Включить плагин |
| `system_prompt` | string | `""` | Инструкция для LLM-улучшения |
| `model_override` | string | `""` | Модель для улучшения (пусто = модель из запроса) |
| `timeout_seconds` | int | `10` | Максимум времени на улучшение |
| `provider_base_url` | string | `https://api.openai.com/v1` | URL API провайдера |
| `provider_api_key` | string | `""` | API-ключ (`env.VAR` для переменных окружения) |

## Как это работает

1. Запрос приходит в Bifrost
2. `PreLLMHook` извлекает последнее сообщение пользователя
3. Отправляет его в LLM с `system_prompt` (улучшающая инструкция)
4. Если ответил вовремя — заменяет сообщение на улучшенное
5. Если таймаут или ошибка — запрос проходит как есть

**Плагин никогда не блокирует запрос.** Он только улучшает.

## Отмена при отключении клиента

1. Клиент нажимает «Отмена» (Copilot)
2. `BifrostContext.Done()` срабатывает
3. `http.NewRequestWithContext` прерывает HTTP-запрос к улучшителю
4. Плагин возвращает оригинальный запрос — токены не потрачены
5. Основной LLM-вызов не начинается

## Сборка

Требуется Go 1.26.4 (как у Bifrost).

```bash
make plugin       # собрать для текущей архитектуры
make plugin-docker # собрать через Docker (для NAS)
make bifrost-dynamic # пересобрать Bifrost с поддержкой плагинов
```

## Структура проекта

```
llmbooster/
├── .github/README.md          # Этот файл
├── .gitmodules                # Bifrost submodule
├── lib/bifrost/               # Bifrost (git submodule)
├── plugins/mcpfilter/
│   ├── main.go                # Реализация плагина
│   ├── go.mod                 # Go module
│   └── README.md              # Документация плагина
├── .agents/skills/
│   └── code-simplification/   # Agent skill
└── talks/                     # Исследования
```

## Лицензия

MIT
