.PHONY: build build-linux build-windows clean install

BINARY_NAME=whelm-voice-server
VERSION?=dev

build:
	go build -ldflags="-s -w -X main.Version=$(VERSION)" -o $(BINARY_NAME) .

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BINARY_NAME)-linux-amd64 .

build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o $(BINARY_NAME)-windows.exe .

build-all: build-linux build-windows

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-linux-* $(BINARY_NAME)-windows*

install: build-linux
	sudo cp $(BINARY_NAME)-linux-amd64 /usr/local/bin/$(BINARY_NAME)
	sudo chmod +x /usr/local/bin/$(BINARY_NAME)
	@echo "Installed to /usr/local/bin/$(BINARY_NAME)"
	@echo "Don't forget to:"
	@echo "  1. Create user: sudo useradd -r -s /bin/false whelm"
	@echo "  2. Create dir: sudo mkdir -p /opt/whelm"
	@echo "  3. Copy service: sudo cp whelm-voice.service /etc/systemd/system/"
	@echo "  4. Edit DSN in /etc/systemd/system/whelm-voice.service"
	@echo "  5. Enable service: sudo systemctl enable --now whelm-voice"

test:
	go test -v ./...

run:
	go run .
