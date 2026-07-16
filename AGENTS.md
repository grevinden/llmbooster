## глоссарий
- `bifrost` - базовое приложение `lib/bifrost`
- `mcpfilter` - плагин который мы разрабарываем
- `mcpfilter.config` - объект настройки плагина который дает пользователь в web-интерфейсе настройки плагина
- `context_mcpfilter` - контекст который используется для улучшения запроса пользователя
- `ответ` - генерация в ответ на `context_mcpfilter`
- `context` - запрос который пришел от клиента в `bifrost`, мутируется в `mcpfilter`, в него может быть добавлено только `ответ`
- `llm` - модель провайдера к которой пришел запрос
- `credentials` - headers запроса, параметры аутентификации могут быть любыми заголовками запроса
- `timeout` сколько можно ждать подзапрос к `llm`
- `system prompt` который использует подзапрос `llm`

## схема пользовательских настроект mcpfilter
- `mcpfilter.config.timeout` - `timeout`
- `mcpfilter.config.system` - `system prompt`

## схема работы `mcpfilter`
- клиент выполняет запрос в `bifrost`
- `mcpfilter` перехватывает запрос клиента `PreRequestHook`
- удаляет из `context` **ВСЕ** пустые сообщения независимо от `role`
- если `context.messages[-1].role` == `user`
  - копирует `context` для мутации в новый `context_mcpfilter` без `messages[...].role` == `system`
    - вставляет первое сообщение в `context_mcpfilter.messages[0]`+`role=system` с текстом который указан пользователем `mcpfilter.config.system`
    - использует `llm-client` из запроса, тоесть `mcpfilter` не имеет собственных настроек `llm`+`credentials`
    - отправляет в `llm-client` весь `context_mcpfilter` !!! `llm-client.timeout`=`mcpfilter.config.timeout`
    - при любых ошибках или `timeout`, эта ветка не влияет на работу
      то-есть эта ветка `mcpfilter` ничего больше не сделала
    - если `llm` вернула не пустой `ответ`
      - **добавляем** `ответ` в `context` после `context.messages[-1]`, `ответ` не изменяем (так как дала `llm`)
- возвращаем мутированный `context` в `bifrost`
