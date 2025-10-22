FROM golang:latest as build

WORKDIR /go/src/app

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-buildid=' -o /go/bin/app .

FROM gcr.io/distroless/static-debian12

COPY --from=build /go/bin/app /
COPY --from=build /go/src/app/host.key /

EXPOSE 22

CMD ["/app", "-addr", ":22", "-host-key", "/host.key"]
