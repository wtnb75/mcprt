package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Message string `json:"message"`
}

type echoOutput struct {
	Message string `json:"message"`
}

func echo(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
	return nil, echoOutput{Message: in.Message}, nil
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes the message"}, echo)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
