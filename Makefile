REPO=ghcr.io/ytsaurus

TARGET_OS=linux
TARGET_ARCH=amd64

ifndef RELEASE_VERSION
RELEASE_VERSION = 0.0.0
endif

.PHONY: test
test:
	cd server/pkg && go test

.PHONY: build
build:
	cd server && GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build -o server . && cd ..

.PHONY: image
image: build
	docker build --platform $(TARGET_OS)/$(TARGET_ARCH) . -t $(REPO)/task-proxy:$(RELEASE_VERSION)

.PHONY: helm-chart
helm-chart: image
	helm package chart --version $(RELEASE_VERSION) --app-version $(RELEASE_VERSION)

.PHONY: release
release: helm-chart
	docker push $(REPO)/task-proxy:$(RELEASE_VERSION)
	helm push task-proxy-chart-$(RELEASE_VERSION).tgz oci://$(REPO)
