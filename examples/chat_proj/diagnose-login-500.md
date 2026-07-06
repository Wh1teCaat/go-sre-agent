# Diagnose Login 500

Goal:

```text
诊断 chat_proj 登录接口 /v1/user/login 为什么返回 500
```

Planned evidence path:

- Check backend health with `http_check`.
- Reproduce login failure with `http_check`.
- Check PostgreSQL with `postgres_ping`.
- Check Redis with `redis_ping`.
- Read recent application logs with `log_read`.
- Generate a markdown diagnosis with evidence and recommendations.
