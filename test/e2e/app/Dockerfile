FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /app main.go

FROM alpine:3.22
ARG VERSION=dev
ENV VERSION=$VERSION
COPY --from=build /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
