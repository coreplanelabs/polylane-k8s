# fakeplatform: hermetic Polylane registration-API simulator for the dev
# environment and e2e tests. Never shipped.
FROM golang:1.27@sha256:921f61120011c774a7b37fd80bd3c3d259636090d3cd002b6a344322a0328082 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/fakeplatform ./cmd/fakeplatform

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/fakeplatform /fakeplatform
EXPOSE 8180
ENTRYPOINT ["/fakeplatform"]
