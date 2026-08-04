BINARY := exe

.PHONY: build install clean

build:
	CGO_ENABLED=1 go build -o $(BINARY) ./cmd/exe
	codesign --entitlements vz.entitlements --force -s - $(BINARY)

install: build
	mkdir -p $(HOME)/bin
	cp $(BINARY) $(HOME)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
