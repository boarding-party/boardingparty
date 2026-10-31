.PHONY: test build image

test:
	go test ./...

build:
	go build ./...

image:
	docker build -f Dockerfile.director -t boardingparty-director:dev .
