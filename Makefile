.PHONY: test build image

test:
	go test ./...

build:
	go build ./...

image:
	@echo "image: filled in by later tasks"
