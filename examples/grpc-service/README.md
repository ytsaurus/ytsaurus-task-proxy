# Sample gRPC server

Code from this example allows to create binary with sample gRPC server, implementing simple helloworld gRPC service.

Build binary and upload it to YTsaurus:
```sh
GOOS=linux GOARCH=amd64 go build .
cat yt-sample-grpc-service | yt upload --executable //home/yt-sample-grpc-service
```
