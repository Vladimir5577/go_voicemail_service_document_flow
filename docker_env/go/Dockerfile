
# Финальный образ (Runner)
FROM debian:bookworm-slim

WORKDIR /app

# Порт микросервиса. Наружу не публикуется: ходит только шлюз по voicemail-net
EXPOSE 8089

# Запуск приложения (бинарник main, собранный на хосте: go build cmd/main.go)
CMD ["./main"]
