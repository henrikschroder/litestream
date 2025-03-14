#!/bin/bash
set -e

# Define version if not provided
if [ -z "$LITESTREAM_VERSION" ]; then
  LITESTREAM_VERSION="latest"
  echo "LITESTREAM_VERSION not set, using 'latest'"
fi

# Build compiler docker image
build_compiler() {
  echo "Building compilation image..."

  # Create a named volume for Go dependencies if it doesn't exist
  docker volume inspect litestream-go-deps >/dev/null 2>&1 || docker volume create litestream-go-deps

  # create the build image
  docker build --platform=linux/amd64 -t litestreambuilder .
}

# Build for Windows
build_windows(){
  echo "Building Windows binary..."
  docker run --rm \
    -v "${PWD}/..":/usr/src/litestream \
    -v litestream-go-deps:/go \
    -w /usr/src/litestream \
    -e GOOS=windows \
    -e GOARCH=amd64 \
    -e CGO_ENABLED=1 \
    -e CC=x86_64-w64-mingw32-gcc \
    litestreambuilder \
    go build -buildvcs=false -ldflags "-s -w -X 'main.Version=${LITESTREAM_VERSION}' -extldflags '-static'" -tags osusergo,netgo,sqlite_omit_load_extension -o ./build/litestream.exe ./cmd/litestream
}


# Build Linux distribution
build_linux() {
  echo "Building Linux distribution..."
  docker run --rm \
    -v "${PWD}/..":/usr/src/litestream \
    -v litestream-go-deps:/go \
    -w /usr/src/litestream \
    -e CGO_ENABLED=1 \
    -e CC=/usr/bin/gcc \
    -e GOOS=linux \
    -e GOARCH=amd64 \
    litestreambuilder \
    go build -buildvcs=false -ldflags "-s -w -X 'main.Version=${LITESTREAM_VERSION}' -extldflags '-static'" -tags osusergo,netgo,sqlite_omit_load_extension -o ./build/litestream.linux ./cmd/litestream
}

# Display usage information
usage() {
  echo "Usage: $0 [command]"
  echo "Commands:"
  echo "  windows       Build Windows binary (default)"
  echo "  docker        Build Docker image"
  echo "  linux         Build Linux distribution"
  echo "  all           Build all distributions"
  echo "  clean         Clean distribution directory"
  echo "  help          Display this help message"
}

# Parse command line arguments
if [ $# -eq 0 ]; then
  # Default is to build Windows only
  echo "No command specified, only Windows binary was built"
  echo "Run '$0 help' for usage information"
else
  case "$1" in
    windows)
      # Windows already built above
      build_compiler
      build_windows
      ;;
    linux)
      build_compiler
      build_linux
      ;;
    all)
      build_compiler
      build_windows
      build_linux
      ;;
    help)
      usage
      ;;
    *)
      echo "Unknown command: $1"
      usage
      exit 1
      ;;
  esac
fi
