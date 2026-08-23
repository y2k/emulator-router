BINARY := emulator-router
PKG    := ./cmd/emulator-router

.PHONY: all build test emulators vet fmt run clean

all: vet test build

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test ./...

# Тесты на настоящих инстансах: занимают слоты в пределе на минуты, поэтому
# отдельно от обычного прогона. Образ задаётся: make emulators AVD=my_avd
emulators:
	go test -tags emulators -count=1 -timeout 20m -run TestLive $(PKG) -avd "$(AVD)" -v

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run $(PKG) $(ARGS)

clean:
	rm -rf bin
