FROM debian:stretch
LABEL crosscompie={pi-arm64}

ENV GOOS=linux
ENV GOARCH=arm
ENV CC="arm-linux-gnueabihf-gcc -O3 -march=armv8-a"
ENV CXX="arm-linux-gnueabihf-g++ -O3 -march=armv8-a"
ENV CGO_ENABLED=1

RUN apt-get update -y && \
    apt-get install -y git && \
    apt-get install -y build-essential && \
    apt-get install -y pkg-config && \
    apt-get install -y upx && \
    apt-get install -y zip && \
    apt-get install -y wget

# for building raspberry pi firmware
RUN echo "Download raspberrypi tools......"
RUN git clone --progress --verbose https://github.com/raspberrypi/tools.git --depth=1 pitools
ENV PATH "/pitools/arm-bcm2708/gcc-linaro-arm-linux-gnueabihf-raspbian-x64/bin:$PATH"

# positioning strip
RUN ln -s `which arm-linux-gnueabihf-strip` /pitools/arm-bcm2708/gcc-linaro-arm-linux-gnueabihf-raspbian-x64/bin/strip

# install golang
RUN echo "Build and install golang......"
RUN wget https://dl.google.com/go/go1.15.5.linux-amd64.tar.gz && \
    [ "9a58494e8da722c3aef248c9227b0e9c528c7318309827780f16220998180a0d" = "$(sha256sum go1.15.5.linux-amd64.tar.gz | cut -d ' ' -f1)" ] && \
    tar -xvf go1.15.5.linux-amd64.tar.gz
RUN mv go /usr/local
ENV GOROOT "/usr/local/go"
RUN mkdir /go
ENV GOPATH "/go"
ENV PATH "$GOPATH/bin:$GOROOT/bin:$PATH"

RUN mkdir build
WORKDIR /build

# OpenSSL Settings
ENV MACHINE armv7l
ENV AR "arm-linux-gnueabihf-ar"
ENV RANLIB "arm-linux-gnueabihf-ranlib"
ENV CROSS_COMPILE "/pitools/arm-bcm2708/gcc-linaro-arm-linux-gnueabihf-raspbian-x64/bin/"

RUN mkdir diode_client
WORKDIR /build/diode_client

COPY . .
RUN make openssl
RUN make archive
