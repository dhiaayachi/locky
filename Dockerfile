# syntax=docker/dockerfile:1

# Build the application from source
FROM golang:1.23 AS build-stage

WORKDIR /app

COPY . .

RUN go mod download


ENTRYPOINT ["go", "run", "-race", "."]
#RUN CGO_ENABLED=0 GOOS=linux go run -race .
## Deploy the application binary into a lean image
#FROM gcr.io/distroless/base-debian12 AS build-release-stage
#
#WORKDIR /
#
#COPY --from=build-stage /locky /locky
#
#EXPOSE 8080
#
#USER nonroot:nonroot
#
#ENTRYPOINT ["/locky"]