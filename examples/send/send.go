package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jech/gclient"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
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
	flag.StringVar(&username, "username", "send-example",
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
				case "failed":
					log.Printf("Join failed: %v", e.Value)
					break outer
				case "join":
					go sendAudio(client)
				}
			case gclient.AbortEvent:
				log.Println("The server refused our data")
				break outer
			case gclient.UserMessageEvent:
				if e.Kind == "error" || e.Kind == "warning" {
					log.Printf("%v: %v", e.Kind, e.Value)
				}
			case gclient.DownConnEvent:
				client.Abort(e.Id)
			case error:
				log.Println(e)
				break outer
			}
		}
	}
	client.Close()
}

func sendAudio(client *gclient.Client) error {
	pc, err := webrtc.NewPeerConnection(*client.RTCConfiguration())
	if err != nil {
		return err
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"},
		"audio", "send-example",
	)
	if err != nil {
		return err
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}

	go func(sender *webrtc.RTPSender) {
		buf := make([]byte, 2048)
		for {
			_, _, err := sender.Read(buf)
			if err != nil {
				if err == io.EOF ||
					errors.Is(err, io.ErrClosedPipe) {
					return
				}
				log.Printf("Read RTCP: %v", err)
				time.Sleep(time.Second)
			}
		}
	}(sender)

	connected := make(chan struct{})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected {
			close(connected)
		}
	})

	id := gclient.MakeId()

	err = client.NewUpConn(id, pc, "camera")
	if err != nil {
		return err
	}
	defer client.CloseUpConn(id)

	<-connected
	log.Println("Peer connection connected, sending data")

	sample := make([]byte, 72)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := track.WriteSample(media.Sample{
			Data: sample,
			Timestamp: time.Now(),
			Duration: 20 * time.Millisecond,
		})
		if err != nil {
			log.Printf("WriteSample: %v", err)
		}
		<-ticker.C
	}
}
