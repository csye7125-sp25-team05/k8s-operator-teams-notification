FROM golang:1.21

WORKDIR /app

COPY go.mod .
COPY go.sum .
COPY *.go .

ENV GO111MODULE=on

RUN go mod download
RUN go build -v -o main main.go
RUN ls -la /app  # Verify the binary exists

# Make the binary executable
RUN chmod +x /app/main

# Run the binary
CMD ["/app/main"]