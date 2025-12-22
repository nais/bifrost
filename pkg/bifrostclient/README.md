# Bifrost Client

Go client for the Bifrost API, generated from the OpenAPI specification.

## Installation

```bash
go get github.com/nais/bifrost/pkg/bifrostclient
```

## Usage

```go
package main

import (
  "context"
  "log"

  "github.com/nais/bifrost/pkg/bifrostclient"
)

func main() {
  ctx := context.Background()

  client, err := bifrostclient.NewClientWithResponses("https://bifrost.example.com/v1")
  if err != nil {
    log.Fatal(err)
  }

  // List all release channels
  channels, err := client.ListChannelsWithResponse(ctx)
  if err != nil {
    log.Fatal(err)
  }
  for _, ch := range *channels.JSON200 {
    log.Printf("Channel: %s, Image: %s", ch.Name, ch.Image)
  }

  // Get a specific Unleash instance
  instance, err := client.GetInstanceWithResponse(ctx, "my-unleash")
  if err != nil {
    log.Fatal(err)
  }
  if instance.JSON200 != nil {
    log.Printf("Instance: %s", *instance.JSON200.Metadata.Name)
  }
}
```

## Regenerating

The client is generated from `api/openapi.yaml`:

```bash
mise run openapi-client
```
