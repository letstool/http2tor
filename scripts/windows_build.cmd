@echo off
go build ^
    -trimpath ^
    -ldflags="-s -w" ^
    -o .\out\http2tor.exe .\cmd\http2tor
