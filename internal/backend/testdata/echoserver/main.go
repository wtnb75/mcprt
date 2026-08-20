package main

import (
	"context"
	"log"
	"os"

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

type cwdOutput struct {
	Dir string `json:"dir"`
}

func cwd(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, cwdOutput, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, cwdOutput{}, err
	}
	return nil, cwdOutput{Dir: dir}, nil
}

type envInput struct {
	Name string `json:"name"`
}

type envOutput struct {
	Value string `json:"value"`
}

func getenv(ctx context.Context, req *mcp.CallToolRequest, in envInput) (*mcp.CallToolResult, envOutput, error) {
	return nil, envOutput{Value: os.Getenv(in.Name)}, nil
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "echoes the message"}, echo)
	mcp.AddTool(srv, &mcp.Tool{Name: "cwd", Description: "reports the server's working directory"}, cwd)
	mcp.AddTool(srv, &mcp.Tool{Name: "env", Description: "reports the value of an environment variable"}, getenv)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
