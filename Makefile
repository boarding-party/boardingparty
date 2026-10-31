.PHONY: test build image image-director image-worker

test:
	go test ./...

build:
	go build ./...

image: image-director image-worker

image-director:
	docker build -f Dockerfile.director -t boardingparty-director:dev .

image-worker:
	docker build -t boardingparty-worker:dev worker/
