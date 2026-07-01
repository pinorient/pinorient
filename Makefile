.PHONY: build run test clean tidy

APP_NAME := geocoder-pb

build:
	go build -o $(APP_NAME) ./cmd/$(APP_NAME)

run: build
	./$(APP_NAME) serve

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -f $(APP_NAME)
	rm -rf pb_data data
