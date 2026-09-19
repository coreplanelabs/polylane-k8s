# fakeplatform: hermetic Polylane registration-API simulator for the dev
# environment and e2e tests. Never shipped.
FROM golang:1.27@sha256:1cfcdb11f37fce9429f617100f39e0251748bbaba454bd431275155701765058 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/fakeplatform ./cmd/fakeplatform

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/fakeplatform /fakeplatform
EXPOSE 8180
ENTRYPOINT ["/fakeplatform"]
