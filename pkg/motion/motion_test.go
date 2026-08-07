package motion

import (
	"math"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	m := New()
	if m == nil {
		t.Fatal("New() returned nil")
	}
	// Should start with zero target and env.
	if m.target != 0 {
		t.Errorf("initial target = %f, want 0", m.target)
	}
	if m.env != 0 {
		t.Errorf("initial env = %f, want 0", m.env)
	}
}

func TestSetLevel(t *testing.T) {
	m := New()
	m.SetLevel(0.5)
	if m.target != 0.5 {
		t.Errorf("target = %f, want 0.5", m.target)
	}
	// Clamping.
	m.SetLevel(2.0)
	if m.target != 1.0 {
		t.Errorf("target = %f, want 1.0 (clamped)", m.target)
	}
	m.SetLevel(-0.5)
	if m.target != 0 {
		t.Errorf("target = %f, want 0 (clamped)", m.target)
	}
}

func TestTickIdle(t *testing.T) {
	m := New()
	// Tick a few times at zero level -- should produce valid poses.
	for i := 0; i < 10; i++ {
		p := m.Tick()
		// Head should be a valid SE3 matrix (last row = [0,0,0,1]).
		if p.Head[12] != 0 || p.Head[13] != 0 || p.Head[14] != 0 || p.Head[15] != 1 {
			t.Errorf("tick %d: head[12:16] = %v, want [0,0,0,1]", i, p.Head[12:16])
		}
		// Antennas should be near rest position.
		if math.Abs(p.Antennas[0]-antRestR) > 0.1 {
			t.Errorf("tick %d: right antenna = %f, want near %f", i, p.Antennas[0], antRestR)
		}
		if math.Abs(p.Antennas[1]-antRestL) > 0.1 {
			t.Errorf("tick %d: left antenna = %f, want near %f", i, p.Antennas[1], antRestL)
		}
		// Body yaw should be small.
		if math.Abs(p.BodyYaw) > bodyYawRad+0.01 {
			t.Errorf("tick %d: body yaw = %f, want <= %f", i, p.BodyYaw, bodyYawRad)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTickSpeaking(t *testing.T) {
	m := New()
	m.SetLevel(1.0)
	// Let the VAD gate rise. Sleep 20ms between ticks to match real 50 Hz usage.
	for i := 0; i < 25; i++ { // ~500ms at 20ms ticks
		m.Tick()
		time.Sleep(20 * time.Millisecond)
	}
	// After 500ms of level=1, env should be near 1.
	if m.env < 0.8 {
		t.Errorf("env after 500ms of speech = %f, want > 0.8", m.env)
	}
	// Head should be moving (not at origin).
	p := m.Tick()
	// At least one of pitch/yaw/roll should be non-zero.
	hasMotion := false
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if i == j {
				if math.Abs(p.Head[i*4+j]-1.0) > 0.001 {
					hasMotion = true
				}
			} else {
				if math.Abs(p.Head[i*4+j]) > 0.001 {
					hasMotion = true
				}
			}
		}
	}
	if !hasMotion {
		t.Error("head rotation matrix is identity -- expected motion while speaking")
	}
}

func TestTickDecay(t *testing.T) {
	m := New()
	m.SetLevel(1.0)
	// Let env rise. Sleep 20ms between ticks to match real 50 Hz usage.
	for i := 0; i < 25; i++ {
		m.Tick()
		time.Sleep(20 * time.Millisecond)
	}
	envSpeaking := m.env

	// Cut level to 0 and let it decay.
	m.SetLevel(0)
	for i := 0; i < 50; i++ { // ~1s
		m.Tick()
		time.Sleep(20 * time.Millisecond)
	}
	if m.env > envSpeaking*0.5 {
		t.Errorf("env after 1s of silence = %f, want < %f (should decay)", m.env, envSpeaking*0.5)
	}
}

func TestSE3(t *testing.T) {
	// Identity: no translation, no rotation.
	m := se3(0, 0, 0, 0, 0, 0)
	expected := [16]float64{
		1, 0, 0, 0,
		0, 1, 0, 0,
		0, 0, 1, 0,
		0, 0, 0, 1,
	}
	for i := 0; i < 16; i++ {
		if math.Abs(m[i]-expected[i]) > 1e-10 {
			t.Errorf("se3 identity[%d] = %f, want %f", i, m[i], expected[i])
		}
	}

	// Translation only.
	m = se3(1, 2, 3, 0, 0, 0)
	if m[3] != 1 || m[7] != 2 || m[11] != 3 {
		t.Errorf("se3 translation = [%f,%f,%f], want [1,2,3]", m[3], m[7], m[11])
	}

	// 90-degree yaw rotation.
	m = se3(0, 0, 0, 0, 0, math.Pi/2)
	// cos(90)=0, sin(90)=1
	// Rz(90) = [[0,-1,0],[1,0,0],[0,0,1]]
	if math.Abs(m[0]-0) > 1e-10 || math.Abs(m[1]+1) > 1e-10 {
		t.Errorf("se3 yaw 90: row0 = [%f,%f], want [0,-1]", m[0], m[1])
	}
	if math.Abs(m[4]-1) > 1e-10 || math.Abs(m[5]-0) > 1e-10 {
		t.Errorf("se3 yaw 90: row1 = [%f,%f], want [1,0]", m[4], m[5])
	}
}

func TestClamp(t *testing.T) {
	tests := []struct{ v, lo, hi, want float64 }{
		{0.5, 0, 1, 0.5},
		{2.0, 0, 1, 1.0},
		{-1.0, 0, 1, 0},
		{0, -1, 1, 0},
	}
	for _, tt := range tests {
		got := clamp(tt.v, tt.lo, tt.hi)
		if got != tt.want {
			t.Errorf("clamp(%f, %f, %f) = %f, want %f", tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

func TestSpring(t *testing.T) {
	// Spring at rest should stay at rest.
	pos, vel := spring(0, 0, 0, 0.02)
	if pos != 0 || vel != 0 {
		t.Errorf("spring at rest: pos=%f vel=%f, want 0,0", pos, vel)
	}

	// Spring pulled away should accelerate toward target.
	pos, vel = spring(1.0, 0, 0, 0.02)
	if pos >= 1.0 {
		t.Error("spring should move toward target")
	}
	if vel >= 0 {
		t.Error("spring velocity should be negative (toward target)")
	}
}

func TestConcurrent(t *testing.T) {
	m := New()
	done := make(chan struct{})
	// Tick from one goroutine, set level from another.
	go func() {
		for i := 0; i < 100; i++ {
			m.Tick()
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	go func() {
		for i := 0; i < 50; i++ {
			m.SetLevel(float64(i % 2)) // toggle 0/1
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-done
	// If we get here without a race detector panic, it's fine.
}
