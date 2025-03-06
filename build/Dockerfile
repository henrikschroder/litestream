# Use an official Go runtime as a parent image
FROM golang:1.23

# Install packages for cross-compilation
RUN apt-get update && apt-get install -y \
	mingw-w64 \
	build-essential \
	&& rm -rf /var/lib/apt/lists/*

# Set the working directory inside the container
WORKDIR /usr/src/litestream