package transcript

// Made-up captions in YouTube's shapes. The excerpts keep real tracks' timings, cue
// splits, entities and sentence-break offsets, so the times the tests expect are the ones
// real captions produced; only the words are new.

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

// row is a caption as srv1 writes it: start and duration in seconds, and its text.
type row struct {
	start, dur float64
	text       string
}

// talk is a baking talk, punctuated English ASR, 0:00-1:00.
var talk = []row{
	{0, 3.28, "Would you believe that plain water,"},
	{1.76, 4.72, "salt,"},
	{3.28, 4.24, "and some flour could rise as tall as"},
	{6.48, 3.36, "this?"},
	{7.52, 4.96, "Sure, it&#39;s a simple recipe, but it&#39;s"},
	{9.84, 4.8, "good, right? We we eat lots of it. I"},
	{12.48, 4.2, "love slow mornings. I bake these loaves"},
	{14.64, 3.88, "each Saturday morning, but this is not"},
	{16.68, 4.6, "the most important trick in today&#39;s"},
	{18.52, 5.16, "talk. We&#39;ll also cover the starter,"},
	{21.28, 4.76, "how to fold the dough, shaping, scoring,"},
	{23.68, 4.48, "and more. Today, I&#39;ll walk you through"},
	{26.04, 4, "the steps that matter most when you bake"},
	{28.16, 4.16, "a loaf in a home oven. And these are"},
	{30.04, 4.76, "the ones that will change your bread the"},
	{32.32, 4.56, "most. If you&#39;d like to bake beside me"},
	{34.8, 3.88, "and learn to read the dough by its"},
	{36.88, 3.64, "feel and smell, then sign up for my"},
	{38.68, 4.6, "new small group weekend sourdough"},
	{40.52, 5.12, "lessons. We go through the lot, from"},
	{43.28, 4.56, "feeding your starter and mixing a dough,"},
	{45.64, 5.28, "to stretching, folding, shaping,"},
	{47.84, 5.44, "scoring and baking, and about how the"},
	{50.92, 4.96, "weather and the kitchen itself change"},
	{53.28, 4.88, "how fast a dough will rise. See them"},
	{55.88, 4.48, "all at example.com/bake."},
	{58.16, 4.44, "There&#39;s a link in the notes as well. The"},
	{60.36, 4.96, "first thing to look at is your starter"},
}

var talkChapters = []rustle.Chapter{{At: 0, Title: "Intro"}, {At: 60, Title: "1. Your Starter"}, {At: 154, Title: "2. Cold Proof"}}

// talkVTT is the same talk as rolling WebVTT (YouTube's fmt=vtt): each cue repeats the
// previous line and carries per-word timestamps. \x20 marks YouTube's blank caption rows,
// a line holding one space.
var talkVTT = strings.ReplaceAll(`WEBVTT
Kind: captions
Language: en

00:00:00.000 --> 00:00:01.750 align:start position:0%
\x20
Would<00:00:00.280><c> you</c><00:00:00.440><c> believe</c><00:00:00.800><c> that</c><00:00:01.040><c> plain</c><00:00:01.320><c> water,</c>

00:00:01.750 --> 00:00:01.760 align:start position:0%
Would you believe that plain water,
\x20

00:00:01.760 --> 00:00:03.270 align:start position:0%
Would you believe that plain water,
salt,

00:00:03.270 --> 00:00:03.280 align:start position:0%
salt,
\x20

00:00:03.280 --> 00:00:06.470 align:start position:0%
salt,
and<00:00:03.600><c> some</c><00:00:04.000><c> flour</c><00:00:04.480><c> could</c><00:00:04.960><c> rise</c><00:00:05.360><c> as</c><00:00:05.600><c> tall</c><00:00:06.000><c> as</c>

00:00:06.470 --> 00:00:06.480 align:start position:0%
and some flour could rise as tall as
\x20

00:00:06.480 --> 00:00:07.510 align:start position:0%
and some flour could rise as tall as
this?

00:00:07.510 --> 00:00:07.520 align:start position:0%
this?
\x20

00:00:07.520 --> 00:00:09.830 align:start position:0%
this?
Sure,<00:00:07.880><c> it's</c><00:00:08.160><c> a</c><00:00:08.280><c> simple</c><00:00:08.680><c> recipe,</c><00:00:09.120><c> but</c><00:00:09.360><c> it's</c>

00:00:09.830 --> 00:00:09.840 align:start position:0%
Sure, it's a simple recipe, but it's
\x20

00:00:09.840 --> 00:00:12.470 align:start position:0%
Sure, it's a simple recipe, but it's
good,<00:00:10.240><c> right?</c><00:00:10.680><c> We</c><00:00:10.880><c> we</c><00:00:11.080><c> eat</c><00:00:11.360><c> lots</c><00:00:11.680><c> of</c><00:00:11.880><c> it.</c><00:00:12.160><c> I</c>

00:00:12.470 --> 00:00:12.480 align:start position:0%
good, right? We we eat lots of it. I
\x20

00:00:12.480 --> 00:00:14.630 align:start position:0%
good, right? We we eat lots of it. I
love<00:00:12.720><c> slow</c><00:00:12.960><c> mornings.</c><00:00:13.480><c> I</c><00:00:13.560><c> bake</c><00:00:13.800><c> these</c><00:00:14.120><c> loaves</c>

00:00:14.630 --> 00:00:14.640 align:start position:0%
love slow mornings. I bake these loaves
\x20

00:00:14.640 --> 00:00:16.670 align:start position:0%
love slow mornings. I bake these loaves
each<00:00:14.880><c> Saturday</c><00:00:15.320><c> morning,</c><00:00:15.760><c> but</c><00:00:15.920><c> this</c><00:00:16.160><c> is</c><00:00:16.320><c> not</c>

00:00:16.670 --> 00:00:16.680 align:start position:0%
each Saturday morning, but this is not
\x20

00:00:16.680 --> 00:00:18.510 align:start position:0%
each Saturday morning, but this is not
the<00:00:16.880><c> most</c><00:00:17.120><c> important</c><00:00:17.560><c> trick</c><00:00:17.840><c> in</c><00:00:18.000><c> today's</c>
`, `\x20`, " ")

// unpunctuated is an older ASR track without punctuation or capitals, 0:00-1:00: a
// gardening podcast's intro.
var unpunctuated = []row{
	{1.28, 3.12, "hello friends"},
	{2.8, 3.76, "it&#39;s been a few weeks since we&#39;ve had a"},
	{4.4, 4.4, "quiet episode so i thought i&#39;d answer a"},
	{6.56, 5.95, "few of your questions"},
	{8.8, 10.349, "so let&#39;s get stuck in to today&#39;s show"},
	{12.51, 6.639, "[Music]"},
	{23.519, 3, "do"},
	{30, 4.48, "welcome back i hope you&#39;re all keeping"},
	{31.439, 4.561, "warm and that your gardens are growing"},
	{34.48, 3.52, "and i hope that the frost hasn&#39;t been"},
	{36, 4.32, "too hard on you i&#39;m your host sam"},
	{38, 4.399, "a gardener at the community plot on"},
	{40.32, 3.6, "river lane thank you very much for"},
	{42.399, 3.601, "listening as i said it&#39;s been a few"},
	{43.92, 3.04, "weeks since the last one so thank you"},
	{46, 2.399, "all"},
	{46.96, 2.96, "and yeah thanks again for sticking"},
	{48.399, 2.881, "around it&#39;s been busy here like i said"},
	{49.92, 3.119, "at the start we&#39;ve been out every day"},
	{51.28, 4.08, "since march with the"},
	{53.039, 3.121, "seedlings a lot of the recent shows"},
	{55.36, 4.32, "were"},
	{56.16, 6.64, "visits to other gardens or chats"},
	{59.68, 6.32, "with guests so i do want to say sorry"},
}

// lecture is a woodworking lecture, 12:55-13:55.
var lecture = []row{
	{775.279, 3.281, "sanding a whole tabletop, I don&#39;t care"},
	{776.88, 3.519, "if you work through five or six grits on"},
	{778.56, 2.959, "the way up to the finest one, because"},
	{780.399, 2.401, "every single pass is only going to take"},
	{781.519, 3.041, "away a very thin layer of the old"},
	{782.8, 3.839, "varnish. Nothing is wasted. It levels"},
	{784.56, 3.519, "out. The mistake most people make is to"},
	{786.639, 3.361, "put on one thick coat and hope it will"},
	{788.079, 3.121, "dry in time for the guests on Saturday."},
	{790, 2.8, "But a thick coat stays soft under a"},
	{791.2, 3.92, "skin for days, and every plate and glass"},
	{792.8, 4.4, "that touches it leaves a mark, so three"},
	{795.12, 3.92, "thin coats with a day between each one"},
	{797.2, 3.68, "is still the fastest way to get a table"},
	{799.04, 3.52, "done. People often ask me about wax, and"},
	{800.88, 4.079, "it looks great on oak, sure. My own aunt"},
	{802.56, 5.04, "waxed her desk in one day. So now I&#39;ll"},
	{804.959, 5.201, "introduce a better option. Hardwax oil"},
	{807.6, 4.4, "soaks deep into the wood like oil, then"},
	{810.16, 3.6, "sets hard as wax. I tried it on a bench"},
	{812, 4.639, "in the shed, one that takes a real"},
	{813.76, 4.639, "beating from clamps and chisels every"},
	{816.639, 2.801, "day, and it held up fine. The real test"},
	{818.399, 3.44, "though, was our kitchen table,"},
	{819.44, 4.72, "which takes hot mugs, wet cloths and a"},
	{821.839, 3.761, "toddler with a spoon seven days a week."},
	{824.16, 3.44, "On that table, the hardwax oil really"},
	{825.6, 4.32, "proved itself over a long winter. Let&#39;s"},
	{827.6, 4.72, "open up the drawers, where the older"},
	{829.92, 4.8, "repairs hide, and honestly, uh, I would"},
	{832.32, 6.4, "call a few of them rather creative. The"},
	{834.72, 7.119, "worst of all is this bent nail. Someone"},
}

// hourMark is a lighthouse history talk across the one-hour mark.
var hourMark = []row{
	{3586.16, 4.48, "in the parish logs. The first lights"},
	{3588.48, 4.079, "on this coast were simple fires kept"},
	{3590.64, 4.159, "burning in iron baskets on poles, and"},
	{3592.559, 4, "the wind blew them out. The next step"},
	{3594.799, 4, "was a glass lantern full of candles,"},
	{3596.559, 4.161, "which the keepers trimmed all through"},
	{3598.799, 3.921, "the night, every single hour. Then the"},
	{3600.72, 3.839, "big change came with the oil lamp and"},
	{3602.72, 4.879, "a polished mirror behind the flame,"},
	{3604.559, 4.56, "so the beam went miles out to sea. But"},
	{3607.599, 3.121, "the oil was costly stuff. It was mostly"},
	{3609.119, 2.96, "whale oil in those years, and keepers"},
	{3610.72, 2.96, "skimped. Later on, paraffin took over"},
	{3612.079, 4.561, "because it was cheap and burned with a"},
	{3613.68, 4.8, "steady white flame that ships could see"},
	{3616.64, 3.04, "in the dark. That was a gift. And then"},
	{3618.48, 3.04, "came electric light, powered"},
	{3619.68, 3.76, "at first by noisy diesel engines"},
	{3621.52, 4, "that the keepers had to oil daily"},
}

// lineStart is how every transcript line starts: a time, a space, then speech.
var lineStart = regexp.MustCompile(`^(?:\d+:)?\d{1,2}:\d\d \S`)

func escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// srv1 is YouTube's default timed-text XML, which escapes its already escaped apostrophes.
func srv1(rows []row) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8" ?><transcript>`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<text start="%s" dur="%s">%s</text>`, rustle.NumString(r.start), rustle.NumString(r.dur), escape(r.text))
	}
	b.WriteString("</transcript>")
	return b.String()
}

func track(language, kind string, original bool, name string) rustle.Caption {
	if name == "" {
		name = language
	}
	return rustle.Caption{
		Language: language, Name: name, Kind: kind, Original: original,
		URL: "https://www.youtube.com/api/timedtext?v=x&lang=" + language + "&kind=" + kind,
	}
}

// stamps are the times that lead lines.
func stamps(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i], _, _ = strings.Cut(l, " ")
	}
	return out
}

func cuesOf(rows []row) []Cue {
	cues, err := ParseCaptions(srv1(rows))
	if err != nil {
		panic(err)
	}
	return DropRepeats(cues)
}

// workshop is a made-up 63 minutes of auto-captions: a bike repair workshop that turns to
// another part every two and a half to four and a half minutes, so topic segmentation has
// shifts to find, with the odd "uh", "um" and stutter. Lines break mid-sentence and stay
// on screen into the next, as ASR lines do. Seeded, so every run reads the same hour.
//
// It reproduces the TypeScript generator exactly: the seed is the same 32-bit LCG
// (Math.imul and >>> 0 are uint32 arithmetic), and each product that feeds a sum is
// converted with float64(), which stops Go from fusing it into one rounding where
// JavaScript rounds twice.
func workshop() []row {
	seed := uint32(63)
	random := func() float64 {
		seed = seed*1664525 + 1013904223
		return float64(seed) / (1 << 32)
	}
	pick := func(list []string) string { return list[int(math.Floor(random()*float64(len(list))))] }
	without := func(list []string, drop func(string) bool) []string {
		var out []string
		for _, s := range list {
			if !drop(s) {
				out = append(out, s)
			}
		}
		return out
	}
	var parts [][]string
	for _, nouns := range []string{
		"tyre, inner tube, valve, valve core, patch, rim tape, tyre lever, pump head",
		"brake pad, brake cable, caliper, brake lever, rotor, barrel adjuster, cable housing, pad spring",
		"chain, quick link, chain tool, sprocket, chainring, chain checker, cassette, chain pin",
		"derailleur, limit screw, derailleur hanger, shifter, jockey wheel, gear cable, cable stop, B-screw",
		"spoke, spoke nipple, hub, truing stand, spoke key, quick release, axle, freehub",
		"headset, bottom bracket, bearing, bearing cup, cone, locknut, crank, crank bolt",
		"saddle, seat post, handlebar, stem, cleat, pedal, grip, seat clamp",
		"dynamo, rear light, front lamp, battery, wiring, reflector, light mount, switch",
		"mudguard, mudguard stay, rack, pannier, bottle cage, kickstand, bell, rack bolt",
	} {
		parts = append(parts, strings.Split(nouns, ", "))
	}
	openers := []string{"Now, let's talk about the {a}.", "Okay, next up is the {a}.", "Right, so the {a}.", "Let me show you the {a} on this one."}
	asides := []string{"Does that make sense?", "Okay.", "Right.", "Bear with me a second.", "I'll come back to that later."}
	sentences := []string{
		"So the {a} is the part most people forget about, more than the {b}.",
		"If the {a} feels loose, check the {b} before you buy anything.",
		"You want the {a} snug, but not so tight that the {b} starts to bind.",
		"I've seen people replace a whole {a} when it was the {b} all along.",
		"Most of the time it's the {a}, not the {b}.",
		"Take your time with the {a} and the {b}, because a rushed job here costs you later.",
		"This {a} came in last week, and you can see the {b} is worn right through.",
		"Give the {a} a wiggle and listen for any clicking from the {b}.",
		"Honestly, a cheap {a} is fine, but spend a bit more on the {b}.",
		"Check the {a} every month or so, and look at the {b} whenever you clean the bike.",
		"Don't overtighten the {a}, because it strips easily, and so does the {b}.",
		"The {a} and the {b} wear together, so replace them together.",
		"Hold the {a} still with one hand and turn the {b} with the other.",
		"Take a photo of the {a} before it comes apart, because the {b} only goes back one way.",
		"If you hear a creak, it's usually the {a} or the {b}.",
		"On older bikes the {a} is often seized, so a bit of oil on the {b} helps.",
	}
	short := regexp.MustCompile(`^[a-z']{1,4}$`)
	var rows []row
	var line []string
	at := 0.24
	t, width := at, 40.0
	flush := func() {
		rows = append(rows, row{rustle.MS(at), rustle.MS(math.Min(5, math.Max(2, t-at+0.6+random()))), strings.ReplaceAll(strings.Join(line, " "), "'", "&#39;")})
		line = nil
		at = t
		width = 34 + float64(random()*12)
	}
	say := func(template string, nouns []string) {
		a := pick(nouns)
		b := pick(without(nouns, func(n string) bool { return n == a }))
		pace := 0.9 + float64(random()*0.3)
		words := strings.Split(strings.Replace(strings.Replace(template, "{a}", a, 1), "{b}", b, 1), " ")
		for i, word := range words {
			roll := random()
			spoken := []string{word}
			switch {
			case i == 0:
			case roll < 0.008:
				spoken = []string{"uh", word}
			case roll < 0.012:
				spoken = []string{"um", word}
			case roll < 0.03 && i < len(words)-1 && short.MatchString(word):
				spoken = []string{word, word}
			}
			for _, w := range spoken {
				if len(line) > 0 && float64(rustle.UTF16Len(strings.Join(line, " ")+" "+w)) > width {
					flush()
				}
				line = append(line, w)
				t += float64((0.14 + float64(0.035*float64(rustle.UTF16Len(w)))) * pace)
			}
		}
		if random() < 0.15 {
			t += 0.4 + float64(random()*2)
		}
	}
	for part := 0; t < 3765; {
		part = (part + 1 + int(math.Floor(random()*float64(len(parts)-1)))) % len(parts)
		// A handful of nouns per stretch, so its words recur the way a speaker's do; with all
		// eight, TextTiling found only half the shifts.
		var nouns []string
		for _, n := range parts[part] {
			if random() < 0.55 {
				nouns = append(nouns, n)
			}
		}
		for len(nouns) < 3 {
			nouns = append(nouns, pick(without(parts[part], func(n string) bool { return slices.Contains(nouns, n) })))
		}
		until := t + 150 + float64(random()*120)
		say(pick(openers), nouns)
		for t < until && t < 3765 {
			if random() < 0.08 {
				say(pick(asides), nouns)
			} else {
				say(pick(sentences), nouns)
			}
		}
	}
	flush()
	return rows
}

// workshopXML is the workshop hour as srv1.
var workshopXML = srv1(workshop())
