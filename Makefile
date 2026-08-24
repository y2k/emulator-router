BINARY := emulator-router
PKG    := ./cmd/emulator-router
# Версия сборки: её называет doctor, чтобы подключённый хуком бинарь был опознаваем.
# Пустая величина (сборка без git) в отчёте называется явно, см. doctor.go.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null)

.PHONY: all build test emulators vet fmt fmtcheck run clean

all: vet fmtcheck test build

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) $(PKG)

# С проверкой гонок: в коде есть взаимное исключение между процессами, ожидание
# в цикле и порождение процесса с ожиданием в отдельной горутине.
test:
	go test -race ./...

# Тесты на настоящих инстансах: занимают слоты в пределе на минуты, поэтому
# отдельно от обычного прогона. Образ задаётся: make emulators AVD=my_avd
emulators:
	go test -tags emulators -count=1 -timeout 20m -run TestLive $(PKG) -avd "$(AVD)" -v

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Проверка, а не правка: make fmt чинит файлы молча, поэтому сам по себе он
# неотформатированный код до основной ветки допускает. Непрерывная сборка
# запускает make целиком, так что отдельного шага в её описании не нужно.
fmtcheck:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt-ed:"; echo "$$out"; exit 1; fi

run:
	go run $(PKG) $(ARGS)

clean:
	rm -rf bin
