#!/bin/bash

IMAGE_TAG=letstool/http2tor:latest

docker build \
        -t "$IMAGE_TAG" \
       -f build/Dockerfile \
       .
