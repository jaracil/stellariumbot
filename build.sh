#!/bin/bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=musl-gcc go build -ldflags='-extldflags "-static"' -o stellariumbot
