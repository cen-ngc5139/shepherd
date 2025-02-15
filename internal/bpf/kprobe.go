// SPDX-License-Identifier: Apache-2.0
/* Copyright 2024 Authors of Cilium */

package bpf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"

	pb "github.com/cheggaaa/pb/v3"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sync/errgroup"
)

type kprober struct {
	links []link.Link

	kprobeMulti bool
	kprobeBatch uint
}

type Kprobe struct {
	hookFunc  string // internal use
	HookFuncs []string
	Prog      *ebpf.Program
}

func attachKprobes(ctx context.Context, bar *pb.ProgressBar, kprobes []Kprobe) (links []link.Link, ignored int, err error) {
	links = make([]link.Link, 0, len(kprobes))
	for _, kprobe := range kprobes {
		select {
		case <-ctx.Done():
			return

		default:
		}

		var kp link.Link
		kp, err = link.Kprobe(kprobe.hookFunc, kprobe.Prog, nil)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.EADDRNOTAVAIL) {
				err = fmt.Errorf("opening kprobe %s: %w", kprobe.hookFunc, err)
				return
			} else {
				err = nil
				ignored++
			}
		} else {
			links = append(links, kp)
		}

		bar.Increment()
	}

	return
}

// AttachKprobes attaches kprobes concurrently.
func AttachKprobes(ctx context.Context, bar *pb.ProgressBar, kps []Kprobe, batch uint) (links []link.Link, ignored int) {
	if batch == 0 {
		log.Fatal("--filter-kprobe-batch must be greater than 0")
	}

	var kprobes []Kprobe
	for _, kp := range kps {
		for _, fn := range kp.HookFuncs {
			kprobes = append(kprobes, Kprobe{
				hookFunc: fn,
				Prog:     kp.Prog,
			})
		}
	}

	if len(kprobes) == 0 {
		return
	}

	errg, ctx := errgroup.WithContext(ctx)

	var mu sync.Mutex
	links = make([]link.Link, 0, len(kprobes))

	attaching := func(kprobes []Kprobe) error {
		l, i, e := attachKprobes(ctx, bar, kprobes)
		if e != nil {
			return e
		}

		mu.Lock()
		links = append(links, l...)
		ignored += i
		mu.Unlock()

		return nil
	}

	var i uint
	for i = 0; i+batch < uint(len(kprobes)); i += batch {
		kps := kprobes[i : i+batch]
		errg.Go(func() error {
			return attaching(kps)
		})
	}
	if i < uint(len(kprobes)) {
		kps := kprobes[i:]
		errg.Go(func() error {
			return attaching(kps)
		})
	}

	if err := errg.Wait(); err != nil {
		log.Fatalf("Attaching kprobes: %v\n", err)
	}

	return
}

// DetachKprobes detaches kprobes concurrently.
func (k *kprober) DetachKprobes() {
	log.Println("Detaching kprobes...")

	links := k.links
	bar := pb.StartNew(len(links))
	defer bar.Finish()

	batch := k.kprobeBatch
	if k.kprobeMulti || batch >= uint(len(links)) {
		for _, l := range links {
			_ = l.Close()
			bar.Increment()
		}

		return
	}

	var errg errgroup.Group
	var i uint
	for i = 0; i+batch < uint(len(links)); i += batch {
		l := links[i : i+batch]
		errg.Go(func() error {
			for _, l := range l {
				_ = l.Close()
				bar.Increment()
			}
			return nil
		})
	}
	for ; i < uint(len(links)); i++ {
		_ = links[i].Close()
		bar.Increment()
	}

	_ = errg.Wait()
}

func NewCustomFuncsKprober(manifest map[string]string, coll *ebpf.Collection) *kprober {
	var k kprober
	k.kprobeBatch = uint(len(manifest))

	for progName, targetName := range manifest {
		kp, err := link.Kprobe(targetName, coll.Programs[progName], nil)
		if err != nil {
			log.Fatalf("Opening kprobe %s: %s\n", targetName, err)
		}

		k.links = append(k.links, kp)
	}

	return &k
}

func NewCustomKretprobes(manifest map[string]string, coll *ebpf.Collection) *kprober {
	var k kprober
	k.kprobeBatch = uint(len(manifest))

	for progName, targetName := range manifest {
		kp, err := link.Kretprobe(targetName, coll.Programs[progName], nil)
		if err != nil {
			log.Fatalf("Opening kretprobe %s: %s\n", targetName, err)
		}

		k.links = append(k.links, kp)
	}

	return &k
}
