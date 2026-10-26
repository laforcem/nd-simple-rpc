FROM golang:1.26 AS build
ARG VERSION=dev
RUN apt-get update && apt-get install -y --no-install-recommends zip && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/nd-simple-rpc ./cmd/nd-simple-rpc
# Stock Go (wasip1) builds the plugin; TinyGo is used only if installed.
RUN make -C plugin package VERSION="${VERSION}"
RUN mkdir /state

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/nd-simple-rpc /nd-simple-rpc
COPY --from=build /src/plugin/nd-simple-rpc.ndp /plugin/nd-simple-rpc.ndp
# Empty named volumes at /state inherit this ownership, so the nonroot user can write the DB.
COPY --from=build --chown=65532:65532 /state /state
ENTRYPOINT ["/nd-simple-rpc"]
CMD ["serve"]
