FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /api-gatewahy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /api-gatewahy /api-gatewahy
EXPOSE 8080
ENTRYPOINT ["/api-gatewahy"]
CMD ["serve"]
