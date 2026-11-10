FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sv-multi-agent ./cmd/sv-multi-agent \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/svctl ./cmd/svctl \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/svma ./cmd/svma

FROM debian:bookworm-slim
# Verilator is a local CLI that the service shells out to, so it must live in
# the same image (a separate compose service cannot be used). The package name
# is a build arg so docker-compose can make the install explicit/overridable.
ARG VERILATOR_PKG=verilator
RUN apt-get update && apt-get install -y --no-install-recommends \
      "${VERILATOR_PKG}" make g++ ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && verilator --version && make --version && g++ --version
WORKDIR /app
COPY --from=build /out/sv-multi-agent /app/sv-multi-agent
COPY --from=build /out/svctl /app/svctl
COPY --from=build /out/svma /app/svma
EXPOSE 50050
ENTRYPOINT ["/app/sv-multi-agent"]
