package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"io"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jech/gclient"
	"github.com/pion/webrtc/v4"
	"github.com/pion/rtp"
)

func main() {
	var username, password string
	var insecure bool
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage: %s group\n", os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.StringVar(&username, "username", "receive-example",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&gclient.Debug, "debug", false,
		"enable protocol logging")
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	client := gclient.NewClient()

	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetHTTPClient(&http.Client{
			Transport: t,
		})

		d := *websocket.DefaultDialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetDialer(&d)
	}

	err := client.Connect(context.Background(), flag.Arg(0))
	if err != nil {
		log.Fatalf("Connect: %v", err)
	}

	err = client.Join(
		context.Background(), flag.Arg(0), username, password,
	)
	if err != nil {
		log.Fatalf("Join: %v", err)
	}

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

outer:
	for {
		select {
		case <-terminate:
			break outer
		case e := <-client.EventCh:
			if e == nil {
				break outer
			}
			switch e := e.(type) {
			case gclient.JoinedEvent:
				switch e.Kind {
				case "fail":
					log.Printf("Join failed: %v", e.Value)
					break outer
				case "join", "change":
					client.Request(
						map[string][]string{
							"": []string{"audio"},
						},
					)
				}
			case gclient.UserMessageEvent:
				if e.Kind == "error" || e.Kind == "warning" {
					log.Printf("%v: %v", e.Kind, e.Value)
				}
			case gclient.DownTrackEvent:
				go rtpLoop(e.Track, e.Receiver)
			case error:
				log.Println(e)
				break outer
			}
		}
	}
	client.Close()
}

func rtpLoop(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) error {
	log.Printf("Got track with codec %v", track.Codec().MimeType)
	go func(receiver *webrtc.RTPReceiver) {
		buf := make([]byte, 2048)
		for {
			_, _, err := receiver.Read(buf)
			if err != nil {
				if err == io.EOF {
					return
				}
				log.Printf("Read RTCP: %v", err)
				time.Sleep(time.Second)
			}
		}
	}(receiver)

	buf := make([]byte, 2048)
	var packet rtp.Packet
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("Read: %v", err)
				return err
			}
			return nil
		}

		err = packet.Unmarshal(buf[:n])
		if err != nil {
			log.Printf("Unmarshal: %v", err)
		}
		log.Printf("Got RTP, %v bytes, seqno=%v, ts=%v\n",
			len(packet.Payload),
			packet.SequenceNumber,
			packet.Timestamp,
		)
	}
}
