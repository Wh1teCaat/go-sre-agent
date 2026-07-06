# Diagnose WebSocket

Goal:

```text
诊断 chat_proj WebSocket 为什么连接失败
```

Planned evidence path:

- Check backend health with `http_check`.
- Attempt the WebSocket handshake with `websocket_check`.
- Read recent WebSocket and auth errors with `log_read`.
- Generate a markdown diagnosis with evidence and recommendations.
