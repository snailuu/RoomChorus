.PHONY: deps build app run test clean

BIN := bin/roomchorus
APP_DIR := dist/RoomChorus.app

deps:
	brew install opus pkg-config

build:
	@mkdir -p bin
	go build -ldflags '-extldflags "-Wl,-sectcreate,__TEXT,__info_plist,Info.plist"' -o $(BIN) ./cmd/roomchorus
	codesign -s - --force $(BIN)

app:
	@chmod +x scripts/build-app.sh
	./scripts/build-app.sh

run: app
	./$(APP_DIR)/Contents/MacOS/roomchorus -open

test:
	go test -race -v ./...

clean:
	rm -rf bin dist
