NAME := oci
PACKAGE_NAME := github.com/elesssss/oci
VERSION := $(shell git describe --tags || echo "unknown-version")
COMMIT := $(shell git rev-parse HEAD)
BUILDTIME := $(shell date -u "+%Y-%m-%d %H:%M:%S %Z")
BUILD_DIR := build
VAR_SETTING := -X "$(PACKAGE_NAME)/constant.Version=$(VERSION)" -X "$(PACKAGE_NAME)/constant.Commit=$(COMMIT)" -X "$(PACKAGE_NAME)/constant.BuildTime=$(BUILDTIME)"
GOBUILD = CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -buildid= $(VAR_SETTING)' \
		-o $(BUILD_DIR)

PLATFORM_LIST = \
	linux-amd64 \
	linux-arm64
	
zip_release = $(addsuffix .zip, $(PLATFORM_LIST))


.PHONY: build clean release
normal: clean build

clean:
	@rm -rf $(BUILD_DIR)
	@echo "Cleaning up."

$(zip_release): %.zip : %
	@zip -du $(BUILD_DIR)/$(NAME)-$<-$(VERSION).zip -j -m $(BUILD_DIR)/$</$(NAME)*
	@zip -du $(BUILD_DIR)/$(NAME)-$<-$(VERSION).zip *.ini *.service
	@echo "✅ $(NAME)-$<-$(VERSION).zip"

all: linux-amd64 darwin-amd64 windows-amd64 # Most used

all-arch: $(PLATFORM_LIST)

release: $(zip_release)

build:
	@-mkdir -p $(BUILD_DIR)
	$(GOBUILD)/$(NAME)

linux-amd64:
	mkdir -p $(BUILD_DIR)/$@
	GOARCH=amd64 GOOS=linux $(GOBUILD)/$@/$(NAME)

linux-arm64:
	mkdir -p $(BUILD_DIR)/$@
	GOARCH=arm64 GOOS=linux $(GOBUILD)/$@/$(NAME)
