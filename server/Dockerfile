FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /spot-api .

FROM alpine:3.22
COPY --from=build /spot-api /usr/local/bin/spot-api
RUN mkdir -p /data && chown nobody:nobody /data
USER nobody
ENTRYPOINT ["spot-api"]
