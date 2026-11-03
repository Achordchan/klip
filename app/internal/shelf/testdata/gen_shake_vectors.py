#!/usr/bin/env python3
"""Independent reference of klip/Services/MouseShakeDetector.cs, written
line by line from the C# source, and the traces it is run on. The Go test
compares the Go detector with the vectors written here:

    python3 gen_shake_vectors.py > shake_vectors.json
"""
import json, random

MIN = -(2**62)  # DateTimeOffset.MinValue, as "very long ago" (ms)


class Detector:
    def __init__(self):
        self.window = 750
        self.min_dx = 7
        self.required = 2
        self.reset()

    def reset(self):
        self.samples = []
        self.changes = 0
        self.last_dir = 0
        self.last_change_at = MIN

    def update(self, now, x):
        self.samples.append((now, x))
        while self.samples and (now - self.samples[0][0]) > self.window:
            self.samples.pop(0)
        if len(self.samples) < 3:
            return False
        prev, cur = self.samples[-2], self.samples[-1]
        dx = cur[1] - prev[1]
        if abs(dx) < self.min_dx:
            return False
        d = 1 if dx > 0 else -1
        if self.last_dir == 0:
            self.last_dir = d
            return False
        if d != self.last_dir:
            if (now - self.last_change_at) < 40:
                self.last_dir = d
                return False
            self.changes += 1
            self.last_change_at = now
            self.last_dir = d
            if self.changes >= self.required:
                return True
        return False


def run(trace):
    det = Detector()
    out = []
    for t, x in trace:
        out.append(det.update(t, x))
    return out


def traces():
    rnd = random.Random(20261020)
    out = []
    # a clean shake: 15 px steps every 16 ms, reversing every 5 steps
    t, x, d, tr = 0, 500.0, 1, []
    for i in range(40):
        if i % 5 == 0:
            d = -d
        x += 15 * d
        t += 16
        tr.append((t, x))
    out.append(tr)
    # slow drift: never reaches 7 px a step
    out.append([(i * 16, 500.0 + 3 * i) for i in range(60)])
    # reversals too close together (under 40 ms)
    out.append([(i * 10, 500.0 + (12 if i % 2 else -12)) for i in range(30)])
    # one direction only
    out.append([(i * 16, 100.0 + 20 * i) for i in range(50)])
    # a shake slower than the window, so samples fall out of it
    out.append([(i * 400, 500.0 + (30 if (i // 2) % 2 else -30) * i) for i in range(20)])
    # random walks
    for n in range(60):
        t, x, tr = 0, 800.0, []
        for _ in range(rnd.randint(5, 80)):
            t += rnd.choice([1, 5, 10, 16, 16, 16, 30, 50, 120, 400, 900])
            x += rnd.choice([-40, -20, -12, -8, -6, -2, 0, 2, 6, 7, 8, 12, 20, 40])
            tr.append((t, x))
        out.append(tr)
    return out


if __name__ == "__main__":
    vectors = [{"trace": [[t, x] for t, x in tr], "fires": run(tr)} for tr in traces()]
    json.dump(vectors, open("/dev/stdout", "w"))
