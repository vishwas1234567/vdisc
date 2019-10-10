// Copyright © 2019 NVIDIA Corporation
package blockdev

import (
	"sync"

	"github.com/tnarg/go-tcmu"
)

type cmdPoolReq struct {
	h    tcmu.SCSICmdHandler
	cmd  *tcmu.SCSICmd
	out  chan tcmu.SCSIResponse
	done func()
}

type CmdPool struct {
	wg       *sync.WaitGroup
	requests chan *cmdPoolReq
	done     chan interface{}
}

func NewCmdPool(poolSize int, bufferSize int) *CmdPool {
	p := &CmdPool{
		wg:       &sync.WaitGroup{},
		requests: make(chan *cmdPoolReq),
		done:     make(chan interface{}),
	}
	for i := 0; i < poolSize; i++ {
		p.wg.Add(1)
		go p.worker(bufferSize)
	}
	return p
}

func (p *CmdPool) worker(bufferSize int) {
	defer p.wg.Done()
	buf := make([]byte, bufferSize)

	for {
		select {
		case req := <-p.requests:
			req.cmd.Buf = buf
			rsp, err := req.h.HandleCommand(req.cmd)
			buf = req.cmd.Buf
			if err != nil {
				panic(err)
			}
			req.out <- rsp
			req.done()
		case <-p.done:
			return
		}
	}
}

func (p *CmdPool) DevReady(h tcmu.SCSICmdHandler) tcmu.DevReadyFunc {
	return func(in chan *tcmu.SCSICmd, out chan tcmu.SCSIResponse) error {
		p.wg.Add(1)
		go func(h tcmu.SCSICmdHandler, in chan *tcmu.SCSICmd, out chan tcmu.SCSIResponse) {
			defer p.wg.Done()
			outstanding := &sync.WaitGroup{}
			for {
				select {
				case <-p.done:
					return
				case cmd, ok := <-in:
					if !ok {
						outstanding.Wait()
						close(out)
						return
					}

					req := &cmdPoolReq{
						h:    h,
						cmd:  cmd,
						out:  out,
						done: outstanding.Done,
					}

					outstanding.Add(1)
					select {
					case p.requests <- req:
					case <-p.done:
						return
					}
				}
			}
		}(h, in, out)
		return nil
	}
}

func (p *CmdPool) Close() error {
	close(p.done)
	p.wg.Wait()
	close(p.requests)
	return nil
}
