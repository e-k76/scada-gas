// Command alarmcli is an operator console for the core's alarm API.
//
//	alarmcli list [-min-severity N] [-tag TAG]
//	alarmcli ack  [-user NAME] ALARM_ID...
//	alarmcli ack-all [-user NAME]
//	alarmcli watch
//	alarmcli shell        (interactive: list | ack <id> | ackall | quit)
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/alarm"
)

func ts(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("15:04:05.000")
}

func sev(s scadav1.AlarmSeverity) string { return strings.TrimPrefix(s.String(), "SEVERITY_") }
func st(s scadav1.AlarmState) string     { return strings.TrimPrefix(s.String(), "ALARM_") }

func printAlarms(w io.Writer, alarms []*scadav1.AlarmEvent) {
	if len(alarms) == 0 {
		fmt.Fprintln(w, "no active alarms")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ALARM ID\tSEVERITY\tSTATE\tVALUE\tRAISED\tACK BY\tMESSAGE")
	for _, a := range alarms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f\t%s\t%s\t%s\n",
			a.AlarmId, sev(a.Severity), st(a.State), a.TriggerValue, ts(a.TriggerTimeUtcMs), a.AckByUser, a.Message)
	}
	tw.Flush()
}

type cli struct {
	alarms scadav1.ScadaAlarmServiceClient
	user   string
}

func (c *cli) list(ctx context.Context, tag string, minSev int) error {
	r, err := c.alarms.GetActiveAlarms(ctx, &scadav1.GetActiveAlarmsRequest{TagId: tag, MinSeverity: scadav1.AlarmSeverity(minSev)})
	if err != nil {
		return err
	}
	printAlarms(os.Stdout, r.Alarms)
	return nil
}

func (c *cli) ack(ctx context.Context, id string) error {
	r, err := c.alarms.AcknowledgeAlarm(ctx, &scadav1.AckAlarmRequest{AlarmId: id, User: c.user})
	if err != nil {
		return err
	}
	if !r.Success {
		fmt.Printf("%-24s NOT acknowledged: %s\n", id, r.Message)
		return nil
	}
	fmt.Printf("%-24s acknowledged -> %s\n", id, st(r.Alarm.State))
	return nil
}

func (c *cli) ackAll(ctx context.Context) error {
	r, err := c.alarms.GetActiveAlarms(ctx, &scadav1.GetActiveAlarmsRequest{})
	if err != nil {
		return err
	}
	n := 0
	for _, a := range r.Alarms {
		if a.State == scadav1.AlarmState_ALARM_UNACK_ALARM || a.State == scadav1.AlarmState_ALARM_UNACK_RTN {
			if err := c.ack(ctx, a.AlarmId); err != nil {
				return err
			}
			n++
		}
	}
	fmt.Printf("%d alarm(s) processed\n", n)
	return nil
}

func (c *cli) watch(ctx context.Context) error {
	stream, err := c.alarms.SubscribeAlarmEvents(ctx, &scadav1.SubscribeRequest{SubscriberId: "alarmcli-watch"})
	if err != nil {
		return err
	}
	fmt.Println("watching alarm events (Ctrl-C to stop)")
	for {
		ev, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		fmt.Printf("%s  %-24s %-8s %-10s %.1f  %s\n", ts(alarm.EventTimeMs(ev)), ev.AlarmId, sev(ev.Severity), st(ev.State), ev.TriggerValue, ev.Message)
	}
}

func (c *cli) shell(ctx context.Context) error {
	fmt.Println("alarm shell - commands: list | ack <alarm_id> | ackall | quit")
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("alarm> ")
		if !sc.Scan() {
			fmt.Println()
			return nil
		}
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		var err error
		switch strings.ToLower(f[0]) {
		case "list", "ls":
			err = c.list(ctx, "", 0)
		case "ack":
			if len(f) < 2 {
				fmt.Println("usage: ack <alarm_id>")
				continue
			}
			err = c.ack(ctx, f[1])
		case "ackall":
			err = c.ackAll(ctx)
		case "quit", "exit", "q":
			return nil
		default:
			fmt.Println("unknown command")
		}
		if err != nil {
			fmt.Println("error:", err)
		}
	}
}

func main() {
	defUser := "operator"
	if u, err := user.Current(); err == nil && u.Username != "" {
		defUser = u.Username
	}
	core := flag.String("core", "127.0.0.1:50051", "core gRPC address")
	who := flag.String("user", defUser, "operator name recorded on acknowledgement")
	minSev := flag.Int("min-severity", 0, "list: minimum severity (0 INFO .. 4 CRITICAL)")
	tag := flag.String("tag", "", "list: only alarms of this tag")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: alarmcli [flags] list | ack ID... | ack-all | watch | shell")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	conn, err := grpc.NewClient(*core, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rpcCtx := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 5*time.Second) }

	c := &cli{alarms: scadav1.NewScadaAlarmServiceClient(conn), user: *who}
	args := flag.Args()
	switch args[0] {
	case "list":
		cctx, cancel := rpcCtx()
		defer cancel()
		err = c.list(cctx, *tag, *minSev)
	case "ack":
		if len(args) < 2 {
			flag.Usage()
			os.Exit(2)
		}
		cctx, cancel := rpcCtx()
		defer cancel()
		for _, id := range args[1:] {
			if err = c.ack(cctx, id); err != nil {
				break
			}
		}
	case "ack-all":
		cctx, cancel := rpcCtx()
		defer cancel()
		err = c.ackAll(cctx)
	case "watch":
		err = c.watch(ctx)
	case "shell":
		err = c.shell(ctx)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
