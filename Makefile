module.tar.gz:
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" .
	rm -f $@
	tar czf $@ meta.json windows_autoupdate.exe

.PHONY: setup
setup: clean update-rdk

.PHONY: clean
clean:
	rm -rf module.tar.gz windows_autoupdate.exe

.PHONY: format
format:
	gofmt -w -s .

.PHONY: update-rdk
update-rdk:
	go get go.viam.com/rdk@latest
	go mod tidy
