ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd
FROM ${GO_IMAGE} AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN mkdir /out && go build -mod=readonly -trimpath -o /out/ \
    ./cmd/control ./cmd/paper-apply ./cmd/paper-check ./cmd/paper-nav ./cmd/paper-quote-pin \
    ./cmd/paper-thesis-outcome ./cmd/paper-shadow-outcome ./cmd/research-replay-export ./cmd/research-budget

FROM busybox:latest@sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/control"]
