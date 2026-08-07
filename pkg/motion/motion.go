// Package motion provides a speech-driven motion model for the Reachy Mini.
//
// It computes head pose, antenna angles, and body yaw at 50 Hz from a raw
// 0..1 speech intensity level. The model uses:
//   - A VAD gate that rises fast and releases slowly to prevent chatter
//   - Sum of incommensurate sinusoids per axis for natural head sway
//   - Spring-damper antenna physics (VTuber-style secondary motion)
//   - Body yaw that shrinks while speaking
//
// This matches the browser 3D twins' talkStep in the Small Talk frontend
// and the official Reachy conversation app's speech tapper.
package motion

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	d2r = math.Pi / 180

	breatheZ  = 0.005 // 5 mm vertical bob
	breatheHz = 0.1

	antSwayRad = 15 * d2r // idle antenna sway
	antHz      = 0.5
	antRestR   = -0.1745 // INIT_ANTENNAS_JOINT_POSITIONS
	antRestL   = 0.1745

	bodyYawRad = 2.5 * d2r // slow idle "look around"
	bodyYawHz  = 0.06

	antStiff = 280.0 // antenna spring (~2.7 Hz natural freq)
	antDamp  = 13.0  // underdamped -> a little bounce, then settle
)

// Speech sway: [amplitude, frequencyHz] per axis (official tuning).
var (
	swayPitch = [2]float64{4.5 * d2r, 2.2}  // small fast nods
	swayYaw   = [2]float64{7.5 * d2r, 0.6}  // big slow side-to-side turns
	swayRoll  = [2]float64{2.25 * d2r, 1.3} // subtle tilt
	swayX     = [2]float64{0.0045, 0.35}    // fore-aft drift (m)
	swayY     = [2]float64{0.00375, 0.45}   // lateral drift (m)
	swayZ     = [2]float64{0.00225, 0.25}   // vertical drift (m)
)

// Model turns a raw 0..1 speech level into smooth head/antenna/body targets.
type Model struct {
	mu     sync.Mutex
	target float64 // raw level, set by the audio side
	start  time.Time
	last   time.Time

	env  float64 // VAD gate (0..1)
	loud float64 // smoothed loudness

	phPitch, phYaw, phRoll, phX, phY, phZ float64

	// Antenna spring state [pos, vel] for right + left.
	antPosR, antVelR, antPosL, antVelL float64
}

// Pose is one 50 Hz target for the daemon.
type Pose struct {
	Head     [16]float64 // row-major 4x4 SE3
	Antennas [2]float64  // [right, left] rad
	BodyYaw  float64
}

// New creates a new motion model with random per-axis phases for variety.
func New() *Model {
	now := time.Now()
	r := func() float64 { return rand.Float64() * 2 * math.Pi }
	return &Model{
		start:   now,
		last:    now,
		phPitch: r(), phYaw: r(), phRoll: r(), phX: r(), phY: r(), phZ: r(),
		antPosR: antRestR, antPosL: antRestL,
	}
}

// SetLevel feeds the latest speech intensity (0..1).
func (m *Model) SetLevel(v float64) {
	m.mu.Lock()
	m.target = clamp(v, 0, 1)
	m.mu.Unlock()
}

// Tick advances the talking sway and returns the current pose.
// Call at 50 Hz (every 20ms).
func (m *Model) Tick() Pose {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	dt := math.Min(now.Sub(m.last).Seconds(), 0.05)
	m.last = now
	t := now.Sub(m.start).Seconds()
	lv := m.target

	// VAD gate: rises fast when the voice starts, releases slowly on a pause.
	envTarget, envTau := 0.0, 0.28
	if lv > 0.05 {
		envTarget = 1.0
	}
	if envTarget > m.env {
		envTau = 0.05
	}
	m.env += (envTarget - m.env) * (1 - math.Exp(-dt/envTau))
	m.loud += (lv - m.loud) * (1 - math.Exp(-dt/0.06))
	g := clamp(math.Pow(m.loud, 0.7)*2.3, 0, 1.25) * m.env

	osc := func(a [2]float64, ph float64) float64 {
		return a[0] * g * math.Sin(2*math.Pi*a[1]*t+ph)
	}
	pitch := osc(swayPitch, m.phPitch)
	yaw := osc(swayYaw, m.phYaw)
	roll := osc(swayRoll, m.phRoll)
	x := osc(swayX, m.phX)
	y := osc(swayY, m.phY)
	z := breatheZ*math.Sin(2*math.Pi*breatheHz*t) + osc(swayZ, m.phZ)

	// Antenna targets (idle sway + gentle perk), then spring toward them.
	antB := antSwayRad * math.Sin(2*math.Pi*antHz*t) * (0.3 + 0.7*m.env)
	perk := 5 * d2r * g
	m.antPosR, m.antVelR = spring(m.antPosR, m.antVelR, antRestR-antB-perk, dt)
	m.antPosL, m.antVelL = spring(m.antPosL, m.antVelL, antRestL+antB-perk, dt)

	bodyYaw := bodyYawRad * math.Sin(2*math.Pi*bodyYawHz*t) * (1 - 0.5*m.env)

	return Pose{
		Head:     se3(x, y, z, roll, pitch, yaw),
		Antennas: [2]float64{m.antPosR, m.antPosL},
		BodyYaw:  bodyYaw,
	}
}

func spring(pos, vel, target, dt float64) (float64, float64) {
	vel += ((target-pos)*antStiff - vel*antDamp) * dt
	return pos + vel*dt, vel
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// se3 builds a row-major 4x4 from translation + intrinsic Rz(yaw)*Ry(pitch)*Rx(roll).
func se3(x, y, z, roll, pitch, yaw float64) [16]float64 {
	cr, sr := math.Cos(roll), math.Sin(roll)
	cp, sp := math.Cos(pitch), math.Sin(pitch)
	cy, sy := math.Cos(yaw), math.Sin(yaw)
	return [16]float64{
		cy * cp, cy*sp*sr - sy*cr, cy*sp*cr + sy*sr, x,
		sy * cp, sy*sp*sr + cy*cr, sy*sp*cr - cy*sr, y,
		-sp, cp * sr, cp * cr, z,
		0, 0, 0, 1,
	}
}
