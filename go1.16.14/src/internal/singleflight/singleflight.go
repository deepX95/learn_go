// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package singleflight provides a duplicate function call suppression
// mechanism.
package singleflight

import "sync"

// call is an in-flight or completed singleflight.Do call
type call struct {
	wg sync.WaitGroup

	// These fields are written once before the WaitGroup is done
	// and are only read after the WaitGroup is done.
	// 存储返回值，在wg done之前只会写入一次
	val interface{}
	// 存储返回的错误信息
	err error

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
	mu sync.Mutex       // protects m    互斥锁，保证 m 并发安全
	m  map[string]*call // lazily initialized   懒加载，存储相同的请求，key是相同的请求，value保存调用信息。
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
		return c.val, c.err, true
	}
	// 之前没有这个请求，则需要new一个指针类型
	c := new(call)
	// sync.waitgroup的用法，只有一个请求运行，其他请求等待，所以只需要add(1)
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
// results when they are ready. The second result is true if the function
// will eventually be called, false if it will not (because there is
// a pending request with this key).
func (g *Group) DoChan(key string, fn func() (interface{}, error)) (<-chan Result, bool) {
	// 初始化channel
	ch := make(chan Result, 1)
	g.mu.Lock()
	// 懒加载
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	// 判断是否有相同的请求
	if c, ok := g.m[key]; ok {
		// 相同请求数量+1
		c.dups++
		// 添加等待的chan
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch, false
	}
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	// 开一个写成调用
	go g.doCall(c, key, fn)
	// 返回这个channel等待接收数据
	return ch, true
}

// doCall handles the single call for a key.
func (g *Group) doCall(c *call, key string, fn func() (interface{}, error)) {
	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	// 正常返回的话直接向channel写入数据就可以了
	for _, ch := range c.chans {
		ch <- Result{c.val, c.err, c.dups > 0}
	}
	g.mu.Unlock()
}

// ForgetUnshared tells the singleflight to forget about a key if it is not
// shared with any other goroutines. Future calls to Do for a forgotten key
// will call the function rather than waiting for an earlier call to complete.
// Returns whether the key was forgotten or unknown--that is, whether no
// other goroutines are waiting for the result.
// 释放某个 key 下次调用就不会阻塞等待了
func (g *Group) ForgetUnshared(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.m[key]
	if !ok {
		return true
	}
	if c.dups == 0 {
		delete(g.m, key)
		return true
	}
	return false
}
