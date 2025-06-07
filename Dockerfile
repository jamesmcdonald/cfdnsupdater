FROM golang:1.24 AS builder

WORKDIR /go/src/cfdnsupdater
COPY . .

RUN go get -d -v ./...
RUN go install -v ./...


FROM debian:stable

WORKDIR /app
COPY --from=builder /go/bin/cfdnsupdater .

RUN apt-get update && apt-get -y install ca-certificates && apt-get clean && rm -rf /var/lib/apt/lists/*

EXPOSE 9876
ENTRYPOINT ["./cfdnsupdater"]
