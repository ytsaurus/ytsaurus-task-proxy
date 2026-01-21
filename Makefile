REPO=ghcr.io/ytsaurus

TARGET_OS=linux
TARGET_ARCH=amd64

ifndef RELEASE_VERSION
RELEASE_VERSION = 0.0.0
endif

.PHONY: test
test:
	cd server/pkg && go test

release:
	cd server && GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build -o server . && cd ..
	docker build --platform $(TARGET_OS)/$(TARGET_ARCH) . -t $(REPO)/task-proxy:$(RELEASE_VERSION)
	docker push $(REPO)/task-proxy:$(RELEASE_VERSION)
	helm package chart
	helm push task-proxy-chart-$(RELEASE_VERSION).tgz oci://$(REPO)
