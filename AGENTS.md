плагин называется **llmboster**

используй `golang`

нельзя использовать пути к `bifrost` через локальные каталоги, так как этот каталог существует для спавки на время разработки
репозиторий [grevinden/bifrost](https://github.com/grevinden/bifrost.git) последний комит основной ветки, состояние соответствует каталогу `./bifrost/`

плагин должен быть в `src/main.go`
  в корне проекта `go.mod`, имя модуля `github.com/grevinden/llmbooster`

нужно использовать `PreLLMHook`,`PostLLMHook` и `HTTPTransportStreamChunkHook`
подготовить `Makefile` в корне проекта, цель `build`

тестировать плагин пользователь будет сам на отдельном сервере `bifrost`

## глоссарий
- `bifrost` - базовое приложение `lib/bifrost`
- `llmboster` - плагин который мы разрабарываем
- `llmboster.config` - объект настройки плагина который дает пользователь в web-интерфейсе настройки плагина
- `think_tracker` - sync.Map для хранения буферов чанков, ключ — RequestID
- `chunk_buffer` - накопленный текст из streaming чанков для одного запроса
- `finish_reason` - причина остановки генерации (`"stop"`, `"tool_calls"`, `"length"`)
- `context` - запрос который пришел от клиента в `bifrost`, мутируется в `llmboster`
- `llm` - модель провайдера к которой пришел запрос
- `system prompt` - системный промпт который использует llm для ответа

## схема пользовательских настроек llmboster
- `llmboster.config.prompt` - промпт для думания (строка) — заменяет system, просит модель сформировать развернутое сообщение пользователя в виде мысли

## схема работы `llmboster`

Общая идея: плагин заставляет модель думать «шаг за шагом» через подмену `system prompt`, а затем перехватывает стриминг-ответ и подменяет последний чанк так, чтобы клиент увидел `tool_calls` и продолжил диалог.

Это работает для **обоих** типов запросов:
- Streaming (`ChatCompletionStreamRequest`) — через `HTTPTransportStreamChunkHook`
- Non-streaming (`ChatCompletionRequest`) — через `PostLLMHook`

### Поток выполнения

```
клиент → bifrost
  → PreLLMHook: модифицируем запрос (system prompt + флаг в BifrostContext)
    → LLM: генерирует ответ с мыслями
      ├── streaming: HTTPTransportStreamChunkHook аккумулирует чанки
      │     → на последнем чанке подменяем finish_reason и вставляем tool_calls
      └── non-streaming: PostLLMHook получает полный ответ
            → подменяем finish_reason и вставляем tool_calls
  → клиент: видит tool_calls → извлекает мысль → отправляет обратно как tool_result
    → LLM: получает контекст + мысль → финальный ответ
```

### PreLLMHook

1. Проверить `req.RequestType` — только `ChatCompletionRequest` / `ChatCompletionStreamRequest`
2. Удалить из `req.ChatRequest.Input` все пустые сообщения:
   - `Content == nil`
   - `Content.ContentStr == nil` и `len(Content.ContentBlocks) == 0`
3. Если последний `Input[-1].Role == "user"`:
   - Сохранить в `BifrostContext` флаг: `ctx.SetValue("llmboster-think-mode", true)`
   - Сохранить `RequestID` в контексте: `ctx.SetValue("llmboster-request-id", id)`
   - Сохранить `RequestType` в контексте: `ctx.SetValue("llmboster-request-type", req.RequestType)` (нужен в PostLLMHook, где нет req)
   - **Модифицировать system prompt:**«
     - Взять первый `Input[i]` с `Role == "system"`
     - Если такого нет — создать новый `ChatMessage{Role: "system", Content: ...}` и вставить в начало
     - Добавить к `Content.ContentStr` текст из `llmboster.config.prompt`
     - Например: `"Оригинальный system prompt...\n\nВАЖНО: думай шаг за шагом, запиши свои рассуждения, а затем дай финальный ответ."`
4. Вернуть `(req, nil, nil)` — запрос продолжается к LLM

### HTTPTransportStreamChunkHook

Сигнатура:
```go
func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error)
```

1. Проверить флаг `"llmboster-think-mode"` в `ctx`. Если нет — вернуть `(chunk, nil)` без изменений
2. Взять `RequestID` из `ctx`
3. Получить/создать `chunk_buffer` для этого `RequestID` в `think_tracker` (sync.Map)
4. **Аккумулировать контент:**
   - `chunk.BifrostChatResponse.Choices[0].ChatStreamResponseChoice.Delta.Content`
   - Проверить `chunk.BifrostChatResponse.Choices[0].FinishReason`
5. Если `FinishReason == nil` или `FinishReason == "null"`:
   - Накопить `content` в буфер
   - Вернуть `(chunk, nil)` — чанк идёт клиенту как есть
6. Если `FinishReason == "stop"`:
   - Это **последний чанк**. У `Delta` уже нет `Content` (или есть финальный текст)
   - Создать **новый чанк** который подменяет оригинальный:
     - `finish_reason: "tool_calls"`
     - `delta: {}` (пустой)
     - `tool_calls`: массив с одним элементом:
       ```json
       {
         "index": 0,
         "id": "think_<RequestID>",
         "type": "function",
         "function": {
           "name": "think",
           "arguments": "{\"thought\": \"<весь накопленный текст мыслей>\"}"
         }
       }
       ```
   - Очистить буфер из `think_tracker`
   - Вернуть подменённый чанк
7. Если `FinishReason == "length"` или другой — не подменять, просто вернуть `(chunk, nil)`
8. Очистка: при удалении из `think_tracker` (по `finish_reason` или ошибке) — удалить запись

### PostLLMHook (non-streaming)

Сигнатура:
```go
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error)
```

1. Проверить флаг `"llmboster-think-mode"` в `ctx`. Если нет — вернуть `(resp, bifrostErr, nil)` без изменений
2. Взять `RequestType` из `ctx` — только `ChatCompletionRequest` (не stream). Для streaming запросов `HTTPTransportStreamChunkHook` уже отработал, не подменяем дважды
3. Если `bifrostErr != nil` — пропустить, вернуть как есть
4. Проверить что `resp.ChatResponse.Choices[0].FinishReason == "stop"`
5. Проверить что `resp.ChatResponse.Choices[0].ChatNonStreamResponseChoice.Message` is not nil и `Role == "assistant"`
6. Если в ответе уже есть `ToolCalls` (через `ChatAssistantMessage`) — пропустить, не подменять
7. Извлечь текст из `resp.ChatResponse.Choices[0].ChatNonStreamResponseChoice.Message.Content.ContentStr`
8. **Подменить non-streaming ответ:**
   - `FinishReason` → `"tool_calls"`
   - `Message.Content` → `nil` (убрать текст мысли из сообщения)
   - `Message.ChatAssistantMessage` → `&schemas.ChatAssistantMessage{ToolCalls: [...]}` с `think` tool_call
9. Вернуть `(resp, nil, nil)` — подменённый ответ идёт клиенту

### Важные детали

- **HTTPTransportStreamChunkHook вызывается для КАЖДОГО чанка** в порядке получения
- Последний чанк — это тот, у которого `FinishReason != nil`
- `finish_reason` может прийти в чанке с **пустым** `delta` — это нормально
- Для non-streaming запросов `HTTPTransportStreamChunkHook` **не вызывается** — работает `PostLLMHook`
- Для streaming запросов `PostLLMHook` **тоже вызывается**, но на уже накопленном ответе — поэтому проверяем `RequestType` из контекста, чтобы не подменить дважды
- Для `finish_reason = "tool_calls"` от модели (если у модели есть инструменты) — не подменять, пропустить как есть

### State Management

**ВАЖНО: НЕ хранить буферы чанков в BifrostContext.** BifrostContext предназначен только для маленьких handles.

Правильный паттерн — `think_tracker` как **глобальный state manager**:

```go
var thinkTracker sync.Map  // key: RequestID (string), value: *chunkBuffer

type chunkBuffer struct {
    mu      sync.Mutex
    content strings.Builder
}

func getOrCreateBuffer(reqID string) *chunkBuffer {
    v, _ := thinkTracker.LoadOrStore(reqID, &chunkBuffer{})
    return v.(*chunkBuffer)
}

func deleteBuffer(reqID string) {
    thinkTracker.Delete(reqID)
}
```

### Что видит клиент

Клиент получает **валидный** стриминг-ответ с `finish_reason: "tool_calls"`:

```
data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{"content":"Paris"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{"content":" is the capital of France"},"finish_reason":null}]}
...
data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
```

Последний чанк надо создать с tool_calls внутри:

```go
// Псевдокод создания подменённого чанка
newChunk := &schemas.BifrostStreamChunk{
    BifrostChatResponse: &schemas.BifrostChatResponse{
        Choices: []schemas.BifrostResponseChoice{
            {
                Index: 0,
                FinishReason: ptr("tool_calls"),
                ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
                    Delta: &schemas.ChatStreamResponseChoiceDelta{
                        ToolCalls: []schemas.ChatAssistantMessageToolCall{
                            {
                                Index: 0,
                                ID:    ptr("think_<RequestID>"),
                                Type:  ptr("function"),
                                Function: schemas.ChatAssistantMessageToolCallFunction{
                                    Name:      ptr("think"),
                                    Arguments: `{"thought":"<мысль>"}`,
                                },
                            },
                        },
                    },
                },
            },
        },
    },
}
```

Клиент (если он агент/ассистент) получает `tool_calls`, «вызывает» функцию `think`, извлекает `arguments.thought` и отправляет результат обратно как `role: "tool"` с `tool_call_id: "think_<RequestID>"`. LLM получает расширенный контекст и выдаёт финальный ответ.

---

## Типы bifrost для PreLLMHook

### `BifrostRequest` (L493-546)

```go
type BifrostRequest struct {
    RequestType RequestType

    ListModelsRequest            *BifrostListModelsRequest
    TextCompletionRequest        *BifrostTextCompletionRequest
    ChatRequest                  *BifrostChatRequest          // <-- чат
    ResponsesRequest             *BifrostResponsesRequest
    // ... остальные поля
}
```

### `BifrostChatRequest`

```go
type BifrostChatRequest struct {
    Provider   ModelProvider
    Model      string
    Input      []ChatMessage          // <-- это "context.messages"
    Params     ChatParameters
    Fallbacks  []Fallback
    RawRequestBody []byte
}
```

### `ChatMessage`

```go
type ChatMessage struct {
    Name    string
    Role    ChatMessageRole      // "user" | "assistant" | "system" | "tool" | "developer"
    Content ChatMessageContent   // строка или массив content blocks
}
```

### `ChatMessageRole`

```go
const ChatMessageRoleAssistant   = "assistant"
const ChatMessageRoleUser        = "user"
const ChatMessageRoleSystem      = "system"
const ChatMessageRoleTool        = "tool"
const ChatMessageRoleDeveloper   = "developer"
```

---

## Типы bifrost для HTTPTransportStreamChunkHook

### `BifrostStreamChunk` (L1726-1735)

```go
type BifrostStreamChunk struct {
    *BifrostTextCompletionResponse
    *BifrostChatResponse         // <-- нас интересует это
    *BifrostResponsesStreamResponse
    *BifrostSpeechStreamResponse
    *BifrostTranscriptionStreamResponse
    *BifrostImageGenerationStreamResponse
    *BifrostPassthroughResponse
    *BifrostError
}
```

### `BifrostChatResponse` — путь к чанку

```go
chunk.BifrostChatResponse.Choices[0].FinishReason                    // *string: "stop" | "tool_calls" | "length"
chunk.BifrostChatResponse.Choices[0].ChatStreamResponseChoice.Delta.Content  // *string: контент чанка
chunk.BifrostChatResponse.Choices[0].ChatStreamResponseChoice.Delta.ToolCalls // []ChatAssistantMessageToolCall
```

### `ChatAssistantMessageToolCall`

```go
type ChatAssistantMessageToolCall struct {
    Index    uint16                               `json:"index"`
    Type     *string                              `json:"type,omitempty"`     // "function"
    ID       *string                              `json:"id,omitempty"`       // "think_<request_id>"
    Function ChatAssistantMessageToolCallFunction `json:"function"`
}

type ChatAssistantMessageToolCallFunction struct {
    Name      *string `json:"name"`      // "think"
    Arguments string  `json:"arguments"` // "{\"thought\":\"...\"}"
}
```

### `BifrostResponseChoice`

```go
type BifrostResponseChoice struct {
    Index        int              `json:"index"`
    FinishReason *string          `json:"finish_reason,omitempty"`
    LogProbs     *BifrostLogProbs `json:"logprobs,omitempty"`

    *TextCompletionResponseChoice
    *ChatNonStreamResponseChoice
    *ChatStreamResponseChoice      // <-- для streaming
}
```

### `ChatStreamResponseChoice` и `ChatStreamResponseChoiceDelta`

```go
type ChatStreamResponseChoice struct {
    Delta *ChatStreamResponseChoiceDelta `json:"delta,omitempty"`
}

type ChatStreamResponseChoiceDelta struct {
    Role             *string                          `json:"role,omitempty"`
    Content          *string                          `json:"content,omitempty"`
    Refusal          *string                          `json:"refusal,omitempty"`
    Reasoning        *string                          `json:"reasoning,omitempty"`
    ReasoningDetails []ChatReasoningDetails           `json:"reasoning_details,omitempty"`
    ToolCalls        []ChatAssistantMessageToolCall   `json:"tool_calls,omitempty"`
    // ...
}
```

---

## Сигнатуры хуков

### PreLLMHook (LLMPlugin)

```go
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error)
```

### HTTPTransportStreamChunkHook (HTTPTransportPlugin)

```go
func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error)
```

Возврат:
- `(chunk, nil)` — чанк идёт клиенту
- `(newChunk, nil)` — подменённый чанк идёт клиенту
- `(nil, nil)` — чанк **пропускается** (не идёт клиенту)
- `(nil, error)` — ошибка логируется как warning, чанк идёт клиенту без изменений

---

## Как bifrost загружает .so плагины

### 1. Чтение конфига

Всё начинается с `config.json`. Bifrost парсит секцию `plugins` в `schemas.PluginConfig`:

```json
{
  "plugins": [
    {
      "enabled": true,
      "name": "llmboster",
      "path": "/path/to/llmboster.so",
      "config": { ... }
    }
  ]
}
```

Этот массив попадает в `ConfigData.Plugins` → затем в `Config.PluginConfigs`.

---

### 2. `server.LoadPlugins()` — точка входа

**Файл:** `bifrost/transports/bifrost-http/server/plugins.go`

```go
func (s *BifrostHTTPServer) LoadPlugins(ctx context.Context) error {
    // 1. Загружает built-in плагины (телеметрия, governance, logging, и т.д.)
    if err := s.loadBuiltinPlugins(ctx); err != nil { return err }
    // 2. Загружает custom плагины (с path — .so файлы)
    if err := s.loadCustomPlugins(ctx); err != nil { return err }
    // 3. Сортирует и перестраивает кеши интерфейсов
    s.Config.SortAndRebuildPlugins()
    return nil
}
```

---

### 3. `loadCustomPlugins()` — итерация по PluginConfigs

```go
func (s *BifrostHTTPServer) loadCustomPlugins(ctx context.Context) error {
    for _, cfg := range s.Config.PluginConfigs {
        if lib.IsBuiltinPlugin(cfg.Name) { continue }
        if !cfg.Enabled { continue }

        plugin, err := InstantiatePlugin(ctx, cfg.Name, cfg.Path, cfg.Config, s.Config)
        if err != nil { /* логируем, статус PluginStatusError */ continue }

        s.Config.ReloadPlugin(plugin)
        s.Config.SetPluginOrderInfo(...)
        s.Config.UpdatePluginOverallStatus(...)
    }
}
```

---

### 4. `InstantiatePlugin()` — роутинг: built-in или custom

```go
func InstantiatePlugin(ctx, name, path, pluginConfig, bifrostConfig) (schemas.BasePlugin, error) {
    if path != nil {
        return loadCustomPlugin(ctx, path, pluginConfig, bifrostConfig)  // .so файл
    }
    return loadBuiltinPlugin(ctx, name, pluginConfig, bifrostConfig)      // встроенный
}
```

---

### 5. `loadCustomPlugin()` — plugin.Open + symbol lookup

**Файл:** `bifrost/framework/plugins/soloader.go`

`PluginLoader` — это `SharedObjectPluginLoader`.

```go
func (l *SharedObjectPluginLoader) LoadPlugin(path string, config any) (schemas.BasePlugin, error) {
    // 1. plugin.Open(path)
    // 2. Lookup("Init") — опционально
    // 3. Lookup("GetName") — обязательно
    // 4. Lookup("Cleanup") — обязательно
    // 5. Lookup хуков — опционально:
    //    PreRequestHook, PreLLMHook, PostLLMHook,
    //    PreMCPHook, PostMCPHook,
    //    HTTPTransportPreHook, HTTPTransportPostHook,
    //    HTTPTransportStreamChunkHook, // <-- наш
    //    PreMCPConnectionHook, PostMCPConnectionHook,
    //    Inject
    return &DynamicPlugin{...}, nil
}
```

**Какой символ ищется в .so:**

```go
// symbol lookup по имени функции, экспортированной из main.go
sym, err := pluginObj.Lookup("PreLLMHook")
sym, err := pluginObj.Lookup("HTTPTransportStreamChunkHook")
```

Для этого функция должна быть экспортирована — в Go это означает, что она:
- объявлена в `package main`
- начинается с заглавной буквы

---

### 6-9. Регистрация, кеши интерфейсов, диспатч

`DynamicPlugin` реализует **все** интерфейсы (`LLMPlugin`, `HTTPTransportPlugin`, ...). Если символ не найден в .so — соответствующий хук возвращает no-op.

Для `HTTPTransportStreamChunkHook`:

```go
func (dp *DynamicPlugin) HTTPTransportStreamChunkHook(ctx, req, chunk) (*schemas.BifrostStreamChunk, error) {
    if dp.httpTransportStreamChunkHook == nil {
        return chunk, nil  // no-op
    }
    return dp.httpTransportStreamChunkHook(ctx, req, chunk)
}
```

---

### Резюме: условия загрузки плагина

Чтобы плагин загрузился, нужно **три совпадения:**

| Условие | Что должно совпадать | Если не совпадает |
|---|---|---|
| **Go version** | `go 1.26.4` — версия в `go.mod` плагина == версия, которой собран bifrost | `plugin.Open()` падает: `"plugin was built with a different version of Go"` |
| **Module path** | `require github.com/maximhq/bifrost/core v1.7.1` — тип `schemas.BifrostRequest` идентифицируется полным путём модуля | `plugin.Open()` падает: `"plugin is built from a different module path"` |
| **Сигнатуры функций** | `func PreLLMHook(...)` и `func HTTPTransportStreamChunkHook(...)` — полное совпадение типов | `"failed to cast ... to expected signature"` |

Плюс **регистрация в `config.json`** — без неё до `plugin.Open()` даже не дойдёт.
