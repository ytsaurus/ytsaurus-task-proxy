REPO=ghcr.io/ytsaurus

TARGET_OS=linux
TARGET_ARCH=amd64

ifndef RELEASE_VERSION
RELEASE_VERSION = 0.0.0
endif

export RELEASE_VERSION=$(RELEASE_VERSION)

.PHONY: all
all: compile-server build-server package-chart

.PHONY: compile-server
compile-server:
	@echo "Compiling task proxy server for target $(TARGET_OS)/$(TARGET_ARCH)"
	cd server && GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build . && cd ..

.PHONY: build-server
build-server:
	@echo "Building task proxy server docker"
	docker build --platform $(TARGET_OS)/$(TARGET_ARCH) . -t $(REPO)/task-proxy:$(RELEASE_VERSION)

.PHONY: update-chart-version
update-chart-version:
	@echo "Updating chart version to $(RELEASE_VERSION)"
	yq -i '.version=strenv(RELEASE_VERSION) | .appVersion=strenv(RELEASE_VERSION)' chart/Chart.yaml
	
.PHONY: update-chart-server-version
update-chart-server-version:
	@echo "Updating chart server version to $(RELEASE_VERSION)"
	yq -i '.server.image.tag=strenv(RELEASE_VERSION)' chart/values.yaml

.PHONY: package-chart
package-chart:
	@echo "Package task proxy helm chart, version $(RELEASE_VERSION)"
	helm package chart --version $(RELEASE_VERSION)

.PHONY: push-server
push-server:
	@echo "Pushing task proxy server docker"
	docker push $(REPO)/task-proxy:$(RELEASE_VERSION)

.PHONY: push-chart
push-chart:
	@echo "Pushing task proxy helm chart"
	helm push task-proxy-$(RELEASE_VERSION).tgz oci://$(REPO)

.PHONY: clean
clean:
	@echo "Cleaning files"
	rm task-proxy-$(RELEASE_VERSION).tgz server/server

release:
	cd server && GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) go build . && cd ..
	docker build --platform $(TARGET_OS)/$(TARGET_ARCH) . -t $(REPO)/task-proxy:$(RELEASE_VERSION)
	docker push $(REPO)/task-proxy:$(RELEASE_VERSION)
	helm package chart
	helm push task-proxy-chart-$(RELEASE_VERSION).tgz oci://$(REPO)