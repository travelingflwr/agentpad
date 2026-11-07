PREFIX ?= $(HOME)/.local

.PHONY: build linux install

build:
	go build -o agentpad .

# A Linux build for running agentpad on another machine through Rex.
linux:
	GOOS=linux GOARCH=amd64 go build -o agentpad-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o agentpad-linux-arm64 .

install:
	go build -o $(PREFIX)/bin/agentpad .

