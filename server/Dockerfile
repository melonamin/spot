FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /quick-api .

FROM alpine:3.22
COPY --from=build /quick-api /usr/local/bin/quick-api
USER nobody
ENTRYPOINT ["quick-api"]
