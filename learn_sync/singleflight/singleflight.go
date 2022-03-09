// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package singleflight provides a duplicate function call suppression
// mechanism.
package singleflight // import "golang.org/x/sync_learn/singleflight"

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// errGoexit indicates the runtime.Goexit was called in
// the user given function.
var errGoexit = errors.New("runtime.Goexit was called")

// A panicError is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type panicError struct {
	value interface{}
	stack []byte
}

// Error implements error interface.
func (p *panicError) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

func newPanicError(v interface{}) error {
	stack := debug.Stack()

	// The first line of the stack trace is of the form "goroutine N [status]:"
	// but by the time the panic reaches Do the goroutine may no longer exist
	// and its status will have changed. Trim out the misleading line.
	if line := bytes.IndexByte(stack[:], '\n'); line >= 0 {
		stack = stack[line+1:]
	}
	return &panicError{value: v, stack: stack}
}

// call is an in-flight or completed singleflight.Do call
type call struct {
	wg sync.WaitGroup

	// These fields are written once before the WaitGroup is done
	// and are only read after the WaitGroup is done.
	// 存储返回值，在wg done之前只会写入一次
	val interface{}
	// 存储返回的错误信息
	err error

	// forgotten indicates whether Forget was called with this call's key
	// while the call was still in flight.
	// 标识别是否调用了Forgot方法
	forgotten bool

	// These fields are read and written with the singleflight
	// mutex held before the WaitGroup is done, and are read but
	// not written after the WaitGroup is done.
	// 统计相同请求的次数，在wg done之前写入
	dups int
	// 使用DoChan方法使用，用channel进行通知
	chans []chan<- Result
}

// Group represents a class of work and forms a namespace in
// which units of work can be executed with duplicate suppression.
type Group struct {
	mu sync.Mutex       // protects m   互斥锁，保证m并发安全
	m  map[string]*call // lazily initialized  懒加载，存储相同的请求，key是相同的请求，value保存调用信息
}

// Result holds the results of Do, so they can be passed
// on a channel.
type Result struct {
	Val    interface{} // 存储返回值
	Err    error       // 存储返回的错误信息
	Shared bool        // 标示结果是否是共享结果
}

// Do executes and returns the results of the given function, making
// sure that only one execution is in-flight for a given key at a
// time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
// The return value shared indicates whether v was given to multiple callers.
// 入参：key：标识相同请求，fn：要执行的函数
// 返回值：v: 返回结果 err: 执行的函数错误信息 shard: 是否是共享结果
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	// 代码块加锁
	g.mu.Lock()
	// map进行懒加载
	if g.m == nil {
		// map初始化
		g.m = make(map[string]*call)
	}
	// 判断是否有相同请求
	if c, ok := g.m[key]; ok {
		// 相同请求次数+1
		c.dups++
		// 解锁就好了，只需要等待执行结果了，不会有写入操作了
		g.mu.Unlock()
		// 已有请求在执行，只需要等待就好了
		c.wg.Wait()
		// 区分panic错误和runtime错误
		if e, ok := c.err.(*panicError); ok {
			panic(e)
		} else if c.err == errGoexit {
			runtime.Goexit()
		}
		return c.val, c.err, true
	}
	// 之前没有这个请求，则需要new一个指针类型
	c := new(call)
	// sync_learn.waitgroup的用法，只有一个请求运行，其他请求等待，所以只需要add(1)
	c.wg.Add(1)
	// m赋值
	g.m[key] = c
	// 没有写入操作了，解锁即可
	g.mu.Unlock()
	// 唯一的请求该去执行函数了
	g.doCall(c, key, fn)
	return c.val, c.err, c.dups > 0
}

// DoChan is like Do but returns a channel that will receive the
// results when they are ready.
//
// The returned channel will not be closed.
//异步返回
// 入参数：key：标识相同请求，fn：要执行的函数
// 出参数：<- chan 等待接收结果的channel
func (g *Group) DoChan(key string, fn func() (interface{}, error)) <-chan Result {
	// 初始化channel
	ch := make(chan Result, 1)
	g.mu.Lock()
	// 懒加载
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	// 判断是否有相同的请求
	if c, ok := g.m[key]; ok {
		//相同请求数量+1
		c.dups++
		// 添加等待的chan
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch
	}
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	// 开一个写成调用
	go g.doCall(c, key, fn)
	// 返回这个channel等待接收数据
	return ch
}

// doCall handles the single call for a key.
func (g *Group) doCall(c *call, key string, fn func() (interface{}, error)) {
	// 标识是否正常返回
	normalReturn := false
	// 标识别是否发生panic
	recovered := false

	// use double-defer to distinguish panic from runtime.Goexit,
	// more details see https://golang.org/cl/134395
	defer func() {
		// the given function invoked runtime.Goexit
		// 通过这个来判断是否是runtime导致直接退出了
		if !normalReturn && !recovered {
			// 返回runtime错误信息
			c.err = errGoexit
		}

		c.wg.Done()
		g.mu.Lock()
		defer g.mu.Unlock()
		// 防止重复删除key
		if !c.forgotten {
			delete(g.m, key)
		}
		// 检测是否出现了panic错误
		if e, ok := c.err.(*panicError); ok {
			// In order to prevent the waiting channels from being blocked forever,
			// needs to ensure that this panic cannot be recovered.
			if len(c.chans) > 0 {
				go panic(e)
				select {} // Keep this goroutine around so that it will appear in the crash dump.
			} else {
				panic(e)
			}
		} else if c.err == errGoexit {
			// runtime错误不需要做任何时，已经退出了
			// Already in the process of goexit, no need to call again
		} else {
			// Normal return
			// 正常返回的话直接向channel写入数据就可以了
			for _, ch := range c.chans {
				ch <- Result{c.val, c.err, c.dups > 0}
			}
		}
	}()
	// 使用匿名函数目的是recover住panic，返回信息给上层
	func() {
		defer func() {
			if !normalReturn {
				// Ideally, we would wait to take a stack trace until we've determined
				// whether this is a panic or a runtime.Goexit.
				//
				// Unfortunately, the only way we can distinguish the two is to see
				// whether the recover stopped the goroutine from terminating, and by
				// the time we know that, the part of the stack trace relevant to the
				// panic has been discarded.
				// 发生了panic，我们recover住，然后把错误信息返回给上层
				if r := recover(); r != nil {
					c.err = newPanicError(r)
				}
			}
		}()
		// 执行函数
		c.val, c.err = fn()
		// fn没有发生panic
		normalReturn = true
	}()

	// 区分panic和runtime错误 https://github.com/golang/go/issues/33519

	// 判断执行函数是否发生panic
	if !normalReturn {
		recovered = true
	}
}

// Forget tells the singleflight to forget about a key.  Future calls
// to Do for this key will call the function rather than waiting for
// an earlier call to complete.

// 释放某个 key 下次调用就不会阻塞等待了
func (g *Group) Forget(key string) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		c.forgotten = true
	}
	delete(g.m, key)
	g.mu.Unlock()
}
