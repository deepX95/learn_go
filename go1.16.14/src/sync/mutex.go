// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	// 当前互斥锁的状态信息
	// 0001：是否被加锁
	// 0010：是否有被唤醒
	// 0100：是否处于饥饿模式
	// 剩余29位，用于统计在互斥锁上的等待队列中goroutine数目（waiter）
	state int32
	sema  uint32 // 信号量，用于控制goroutine的阻塞与唤醒
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked   // 0001
	mutexWoken                   // 0010
	mutexStarving                // 0100
	mutexWaiterShift = iota      // mutexWaiterShift值为3，通过右移3位的位运算，可计算waiter个数

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6 // 1ms，进入饥饿状态的等待时间
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	// 尝试自旋竞争或饥饿状态下饥饿goroutine竞争 m.lockSlow()
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64 // 用于计算waiter的等待时间
	starving := false       // 饥饿标志
	awoke := false          // 唤醒标志
	iter := 0               // 统计当前goroutine的自旋次数
	old := m.state          // 保存当前锁的状态
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 判断是否能进入自旋
		// 锁是非饥饿状态，锁还没被释放，尝试自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.

			// !awoke 判断当前goroutine是不是在唤醒状态
			// old&mutexWoken == 0 表示没有其他正在唤醒的goroutine
			// old>>mutexWaiterShift != 0 表示等待队列中有正在等待的goroutine
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				// 尝试将当前锁的低2位的Woken状态位设置为1，表示已被唤醒
				// 这是为了通知在解锁Unlock()中不要再唤醒其他的waiter了
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋
			runtime_doSpin()
			iter++
			old = m.state // 再次获取锁的状态，之后会检查是否锁被释放了
			continue
		}
		// old是锁当前的状态，new是期望的状态，以期于在后面的CAS操作中更改锁的状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {
			// 如果当前锁不是饥饿模式，则将new的低1位的Locked状态位设置为1，表示加锁
			new |= mutexLocked // 非饥饿状态，加锁
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			// 如果当前锁已被加锁或者处于饥饿模式，则将waiter数加1，表示当前goroutine将被作为waiter置于等待队列队尾
			new += 1 << mutexWaiterShift // waiter数量加1
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		if starving && old&mutexLocked != 0 {
			// 如果当前锁处于饥饿模式，并且已被加锁，则将低3位的Starving状态位设置为1，表示饥饿
			new |= mutexStarving // 设置饥饿状态
		}
		// 当awoke为true，则表明当前goroutine在自旋逻辑中，成功修改锁的Woken状态位为1
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 将唤醒标志位Woken置回为0
			// 因为在后续的逻辑中，当前goroutine要么是拿到锁了，要么是被挂起。
			// 如果是挂起状态，那就需要等待其他释放锁的goroutine来唤醒。
			// 假如其他goroutine在unlock的时候发现Woken的位置不是0，则就不会去唤醒，那该goroutine就无法再醒来加锁。
			new &^= mutexWoken // 新状态清除唤醒标记
		}

		// 尝试将锁的状态更新为期望状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 如果锁的原状态既不是被获取状态，也不是处于饥饿模式
			// 那就直接返回，表示当前goroutine已获取到锁
			// 原来锁的状态已释放，并且不是饥饿状态，正常请求到了锁，返回
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}

			// 处理饥饿状态

			// If we were already waiting before, queue at the front of the queue.
			// 如果走到这里，那就证明当前goroutine没有获取到锁
			// 这里判断waitStartTime != 0就证明当前goroutine之前已经等待过了，则需要将其放置在等待队列队头
			// 如果以前就在队列里面，加入到队列头
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				// 如果之前没有等待过，就以现在的时间来初始化设置
				waitStartTime = runtime_nanotime()
			}
			// 阻塞等待
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// 被信号量唤醒之后检查当前goroutine是否应该表示为饥饿
			// （这里表示为饥饿之后，会在下一轮循环中尝试将锁的状态更改为饥饿模式）
			// 1. 如果当前goroutine已经饥饿（在上一次循环中更改了starving为true）
			// 2. 如果当前goroutine已经等待了1ms以上
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 再次获取锁状态
			old = m.state
			// 走到这里，如果此时锁仍然是饥饿模式
			// 因为在饥饿模式下，锁是直接交给唤醒的goroutine
			// 所以，即把锁交给当前goroutine
			// 如果锁已经处于饥饿状态，直接抢到锁，返回
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果当前锁既不是被获取也不是被唤醒状态，或者等待队列为空
				// 这代表锁状态产生了不一致的问题
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 因为当前goroutine已经获取了锁，delta用于将等待队列-1
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 如果当前goroutine中的starving标志不是饥饿
				// 或者当前goroutine已经是等待队列中的最后一个了
				// 就通过delta -= mutexStarving和atomic.AddInt32操作将锁的饥饿状态位设置为0，表示为正常模式
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving  // 最后一个waiter或者已经不饥饿了，清除饥饿标记
				}
				atomic.AddInt32(&m.state, delta)
				// 拿到锁退出，业务逻辑处理完之后，需要调用Mutex.Unlock()方法释放锁
				break
			}
			// 如果锁不是饥饿状态
			// 因为当前goroutine已经被信号量唤醒了
			// 那就将表示当前goroutine状态的awoke设置为true
			// 并且将自旋次数的计数iter重置为0，如果能满足自旋条件，重新自旋等待
			awoke = true
			iter = 0
		} else {
			// 如果CAS未成功,更新锁状态，重新一个大循环
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// new是解锁的期望状态
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 1. 如果Unlock了一个没有上锁的锁，则会发生panic。
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 2. 正常模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.

			// 如果锁没有waiter,或者锁有其他以下已发生的情况之一，则后面的工作就不用做了，直接返回
			// 1. 锁处于锁定状态，表示锁已经被其他goroutine获取了
			// 2. 锁处于被唤醒状态，这表明有等待goroutine被唤醒，不用再尝试唤醒其他goroutine
			// 3. 锁处于饥饿模式，那么锁之后会被直接交给等待队列队头goroutine
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.

			// 如果能走到这，那就是上面的if判断没通过
			// 说明当前锁是空闲状态，但是等待队列中有waiter，且没有goroutine被唤醒
			// 所以，这里我们想要把锁的状态设置为被唤醒，等待队列waiter数-1
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// 通过CAS操作尝试更改锁状态
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 通过信号量唤醒goroutine，然后退出
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			// 这里是CAS失败的逻辑
			// 因为在for循环中，锁的状态有可能已经被改变了，所以这里需要及时更新一下状态信息
			// 以便下个循环里作判断处理
			old = m.state
		}
	} else { // 3. 饥饿模式
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.

		// 因为是饥饿模式，所以非常简单
		// 直接唤醒等待队列队头goroutine即可
		runtime_Semrelease(&m.sema, true, 1)
	}
}
