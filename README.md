<div align="center">
<h1>gRPC Plugins</h1>
<p>
forked from <a href="https://github.com/grpc-ecosystem/grpc-gateway">gRPC-Gateway</a>
</p>
<a href="https://github.com/grpc-ecosystem/grpc-gateway/blob/master/LICENSE.txt"><img src="https://img.shields.io/github/license/grpc-ecosystem/grpc-gateway?color=379c9c&style=flat-square"/></a>
</div>

## About

This repo is forked from the repo gRPC-Gateway, and most of the code has been removed, only the parts related to the [protoc](https://github.com/protocolbuffers/protobuf) plugin are kept.

Plugins list:

- protoc-gen-openapi3 a plugin for generating OAS3 document with the .proto file. 


## Installation

```shell
$ go get -u github.com/dspo/grpc-plugins/protoc-gen-openapi
```

## Usage

See more: [gRPC-Gateway](https://github.com/grpc-ecosystem/grpc-gateway)

7. Generate Openapi3 definitions using `protoc-gen-openapi3`

   With `protoc` (just the Openapi3 file):

   ```sh
   protoc -I . --openapi3_out ./gen/openapi3 \
       --openapi3_opt=output_format=yaml \
       your/service/v1/your_service.proto
   ```