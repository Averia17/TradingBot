ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd
FROM ${GO_IMAGE} AS source
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal

FROM source AS jupiter-build
RUN go build -mod=readonly -trimpath -o /out/jupiter-adapter ./cmd/jupiter-adapter

FROM scratch AS jupiter-adapter
COPY --from=jupiter-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=jupiter-build /out/jupiter-adapter /jupiter-adapter
USER 65532:65532
ENTRYPOINT ["/jupiter-adapter"]

FROM source AS build
RUN go build -mod=readonly -trimpath -o /out/venue-quotes ./cmd/venue-quotes

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/venue-quotes /venue-quotes
USER 65532:65532
ENTRYPOINT ["/venue-quotes"]
