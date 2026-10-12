# One image runs directly on Cloud Run and behind the pinned Lambda Web Adapter on Lambda.
# Built from the repository root: go.mod replaces the protocol module with ../shipper-protocol.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
# What `fleet-manager` with no arguments reports. Left unset, the binary says "dev", which is the
# right answer for a build nobody stamped.
ARG VERSION=dev
WORKDIR /src/fleet-manager
COPY shipper-protocol/ /src/shipper-protocol/
COPY fleet-manager/go.mod fleet-manager/go.sum ./
RUN go mod download
COPY fleet-manager/ .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
	-ldflags "-s -w -X main.version=$VERSION" -o /out/fleet-manager ./src

FROM gcr.io/distroless/static-debian12 AS lambda
COPY --from=public.ecr.aws/awsguru/aws-lambda-adapter:0.9.1 /lambda-adapter /opt/extensions/lambda-adapter
COPY --from=build /out/fleet-manager /fleet-manager
# The binary bundles its dependencies, so their license texts ship beside it (see NOTICE).
COPY --from=build /src/fleet-manager/LICENSE /src/fleet-manager/NOTICE /usr/share/doc/fleet-manager/
COPY --from=build /src/fleet-manager/third_party /usr/share/doc/fleet-manager/third_party
ENV PORT=8080 AWS_LWA_PORT=8080 AWS_LWA_READINESS_CHECK_PATH=/healthz
EXPOSE 8080
ENTRYPOINT ["/fleet-manager"]
CMD ["--serve"]

FROM gcr.io/distroless/static-debian12:nonroot AS cloud-run
COPY --from=build /out/fleet-manager /fleet-manager
COPY --from=build /src/fleet-manager/LICENSE /src/fleet-manager/NOTICE /usr/share/doc/fleet-manager/
COPY --from=build /src/fleet-manager/third_party /usr/share/doc/fleet-manager/third_party
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/fleet-manager"]
CMD ["--serve"]
