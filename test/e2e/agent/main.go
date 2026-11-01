// Command agent calls the core's gRPC API as a caller outside Themis, for the
// end-to-end test. It prints the gRPC code of listing the open incidents.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: agent <address> <token>")
		os.Exit(2)
	}
	conn, err := grpc.NewClient(os.Args[1], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+os.Args[2])
	_, err = themisv1.NewProposalServiceClient(conn).ListIncidents(ctx, &themisv1.ListIncidentsRequest{})
	fmt.Println(status.Code(err))
}
