package main

import (
	"context"
	"log"
	"os"
	"time"

	nanolytica "github.com/Nanolytica/nanolytica-golang-sdk"
)

func main() {
	siteID := os.Getenv("NANOLYTICA_SITE_ID")
	if siteID == "" {
		log.Fatal("NANOLYTICA_SITE_ID env var is required")
	}
	endpoint := os.Getenv("NANOLYTICA_ENDPOINT")

	client, err := nanolytica.New(siteID, &nanolytica.Options{
		Endpoint:  endpoint,
		UserAgent: "MinimalDemo/1.0.0 (Server; linux)",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ctx := context.Background()

	if err := client.Pageview(ctx, "/home", nil); err != nil {
		log.Printf("pageview: %v", err)
	}
	if err := client.Track(ctx, "signup", map[string]string{"plan": "pro"}, nanolytica.Value(49.99)); err != nil {
		log.Printf("track: %v", err)
	}

	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Flush(fctx); err != nil {
		log.Printf("flush: %v", err)
	}
	log.Println("sent 1 pageview + 1 event")
}
