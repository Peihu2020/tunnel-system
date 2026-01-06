package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name:  "tunnelctl",
		Usage: "Manage tunnel connections",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List registered services",
				Action: func(c *cli.Context) error {
					return listServices(c.String("server"))
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "server",
						Value: "https://localhost:8443",
						Usage: "Control server URL",
					},
				},
			},
			{
				Name:  "tunnel",
				Usage: "Create a tunnel to a service",
				Action: func(c *cli.Context) error {
					if c.Args().Len() < 2 {
						return fmt.Errorf("usage: tunnel <service-id> <port>")
					}
					return createTunnel(
						c.String("server"),
						c.Args().Get(0),
						c.Args().Get(1),
					)
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "server",
						Value: "https://localhost:8443",
						Usage: "Control server URL",
					},
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func listServices(serverURL string) error {
	req, err := http.NewRequest("GET", serverURL+"/api/v1/services", nil)
	if err != nil {
		return err
	}

	// Add auth token
	req.Header.Set("Authorization", "Bearer your-token")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var services []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		return err
	}

	data, _ := json.MarshalIndent(services, "", "  ")
	fmt.Println(string(data))

	return nil
}

func createTunnel(serverURL, serviceID, port string) error {
	reqBody := map[string]interface{}{
		"service_id":  serviceID,
		"target_port": port,
	}

	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequest("POST", serverURL+"/api/v1/tunnel", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer your-token")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	fmt.Printf("Tunnel created:\n")
	for k, v := range result {
		fmt.Printf("  %s: %v\n", k, v)
	}

	return nil
}
