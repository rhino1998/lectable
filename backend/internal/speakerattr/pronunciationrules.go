package speakerattr

import (
	"regexp"
	"strings"
	"unicode"
)

// resolveHomographByGrammar picks a homograph term's sense from the words
// immediately around it, without the LLM, when a grammatical cue makes the
// answer near-certain: a modal or "to" before "read" means present tense
// ("can read", "to read"), a perfect/passive auxiliary means past ("had
// read", "be read"), a determiner before "tear" means a noun (and so, most
// often, a teardrop), a degree word before "close" means the adjective
// ("too close"), a possessive before "lives" means the noun. ok is false when no rule is confident; the term then
// goes to the LLM as before.
//
// Benchmarked on hand-labeled terms from this app's own library (a
// held-out set of chapters not used to write the rules): the rules decided
// 100 of 120 terms with 96 correct, where the LLM alone got 89% of the same
// set and the original id/index prompt got under half of a larger set
// right - so the rules both remove most LLM calls and raise accuracy.
//
// text is the term's own paragraph; isQuote is ParagraphInput.IsQuote.
// "read" in narration defaults to past tense - right for the
// past-tense narration nearly all fiction uses, wrong for a present-tense
// novel (see pronunciationNarrationPastTense).
func resolveHomographByGrammar(kind, text string, start, end int, isQuote bool) (choice int, ok bool) {
	prev := wordsBefore(text, start, 2)
	p1, p2 := prev[1], prev[0]
	n1, n2 := nextWords(text[end:])
	glued := strings.HasPrefix(text[end:], "-")

	switch kind {
	case "read":
		switch {
		case isModal(p1) || (softAdverbs[p1] && isModal(p2)):
			return 0, true // can read, to read, will not read
		case perfectOrPassive[p1] || (softAdverbs[p1] && perfectOrPassive[p2]):
			return 1, true // had read, was read, well read
		case strings.HasPrefix(text[end:], ":"):
			return 1, true // the sign read: ...
		case readNounAdjectives[p1]:
			return 0, true // a good read
		case !isQuote && pronunciationNarrationPastTense:
			return 1, true
		}
	case "wind":
		// Verb only with a clear verb frame; the noun is overwhelmingly
		// more common, including in names ("Wind Strike", "Second Wind").
		if determiners[p1] || p1 == "of" || p1 == "the" {
			return 0, true
		}
		if (windParticles[n1] || windObjects[n1]) && (isModal(p1) || p1 == "" || isSubjectPronoun(p1)) {
			return 1, true // will wind the clock, to wind down
		}
		if n1 == "up" || n1 == "down" {
			return 1, true // wind up, wind down
		}
		return 0, true
	case "lead":
		switch {
		case glued || p1 == "as" || p1 == "of" || leadMetalNouns[n1]:
			return 1, true // lead-lined, heavy as lead, made of lead, lead pipe
		case isModal(p1) || determiners[p1]:
			return 0, true // will lead, to lead, the lead, her lead
		case p1 == "" && isCapitalized(text[start:end]):
			return 0, true // "Lead the way."
		}
	case "tear":
		switch {
		case glued:
			return 1, true // tear-stained
		case n1 == "up":
			return 0, false // "eyes tear up" vs "tear up the letter"
		case isModal(p1) || p1 == "and" || p1 == "or":
			return 0, true // would tear, rip and tear, wear and tear
		case determiners[p1] || p1 == "single" || p1 == "lone":
			if n1 == "in" || n1 == "through" || n1 == "into" {
				return 0, true // a tear in the fabric
			}
			return 1, true // a tear rolled down
		}
		return 0, true
	case "close":
		switch {
		case glued:
			return 0, true // close-knit, close-up
		case (p1 == "the" && n1 == "of") || (p1 == "a" && p2 == "to" && n1 == ""):
			return 1, true // the close of day, drew to a close
		case closeDegreeWords[p1]:
			return 0, true // too close, so close, how close
		case isModal(p1) || (softAdverbs[p1] && isModal(p2)):
			return 1, true // to close, will close, did not close
		case closeNearNext[n1]:
			return 0, true // close to, close enough, close friends
		case closeLinkingVerbs[p1]:
			return 0, true // was close, came close, stay close
		case closeObjects[n1]:
			return 1, true // close the door, close her eyes
		case determiners[p1] && n1 != "" && n1 != "of":
			return 0, true // a close shave, her close and constant companion
		case p1 == "in" || p1 == "up":
			return 0, true // in close, up close
		case closeHoldVerbs[p2] && closeHeldPronouns[p1]:
			return 0, true // held her close
		case n1 == "down" || n1 == "off" || n1 == "out":
			return 1, true // close down, close off
		}
	case "wound":
		switch {
		case glued:
			return 1, true // wound-up
		case determiners[p1] || woundAdjectives[p1] || isModal(p1) || strings.HasSuffix(p1, "ing"):
			return 0, true // the wound, a stab wound, a seeping wound, to wound
		case windParticles[n1] || woundParticles[n1]:
			return 1, true // wound around, wound up, wound through
		case (possessives[n1] || n1 == "the" || n1 == "a") && woundObjects[n2]:
			return 1, true // wound its way, wound her arms around him
		case p1 == "tightly" || p1 == "tight" || p1 == "loosely":
			return 1, true // tightly wound
		}
		return 0, true
	case "live":
		switch {
		case glued:
			return 1, true // live-fire
		case isModal(p1) || strings.HasSuffix(p1, "'d") || (softAdverbs[p1] && isModal(p2)):
			return 0, true // to live, will live, he'd live, can't even live
		case liveNouns[n1]:
			return 1, true // live fire, live feed, live rounds
		case p1 == "a" || p1 == "an" || p1 == "the" || p1 == "any" || p1 == "no":
			return 1, true // a live grenade, the live one
		case liveLinkingVerbs[p1] || ((softAdverbs[p1] || p1 == "still") && liveLinkingVerbs[p2]):
			return 1, true // went live, is live, was still live
		case liveVerbNext[n1] || isSubjectPronoun(p1) || liveSubjects[p1]:
			return 0, true // live here, live with it, we live, people live
		case closeHeldPronouns[p1]:
			return 0, true // let him live
		}
	case "lives":
		switch {
		case livesNounBefore[p1]:
			return 0, true // their lives, saved lives, nine lives
		case livesVerbBefore[p1]:
			return 1, true // she lives, who lives, whatever lives there
		case p1 == "":
			if liveVerbNext[n1] {
				return 1, true // "Lives in the hills."
			}
			return 0, false
		case capitalizedBefore(text, start) && !strings.HasSuffix(p1, "'s") && !strings.HasSuffix(p1, "s'") && (n1 == "" || liveVerbNext[n1]):
			return 1, true // Dave lives with me
		}
		return 0, true
	case "minute":
		switch {
		case strings.HasSuffix(text[:start], "-"):
			return 0, true // last-minute changes
		case minuteTinyNouns[n1]:
			return 1, true // minute details, minute adjustments
		case p1 == "most" || p1 == "incredibly" || p1 == "infinitely" || p1 == "extremely" || p1 == "exceedingly":
			return 1, true // the most minute
		case (p1 == "very" || p1 == "so" || p1 == "too") && p2 != "this" && p2 != "that":
			return 1, true // so minute - but "this very minute"
		}
		return 0, true
	case "tears":
		switch {
		case tearsRipFabricRE.MatchString(strings.ToLower(text[end:])):
			return 1, true // tears in the fabric
		case tearsDropBefore[p1]:
			return 0, true // in tears, her tears, into tears
		case tearsRipNext[n1]:
			return 1, true // tears through, tears apart, tears open
		case (isSubjectPronoun(p1) || tearsSubjects[p1]) && (closeObjects[n1] || tearsRipAfterSubject[n1]):
			return 1, true // it tears the page, she tears at it
		}
		return 0, true
	case "bow":
		switch {
		case glued:
			return 0, true // bow-strings
		case p1 == "ship's" || (p1 == "the" && n1 == "of" && n2 == "the"):
			return 1, true // the ship's bow
		case isModal(p1):
			return 1, true // to bow, must bow
		case p1 == "and" && bowGestureVerbs[p2]:
			return 1, true // scrape and bow
		case bowGestureAdjectives[p1]:
			return 1, true // a slight bow
		case bowWeaponAdjectives[p1] || bowWeaponNext[n1]:
			return 0, true // hunting bow, bow and arrow, bow drawn
		case (p1 == "a" || p1 == "an") && (n1 == "then" || n1 == "to" || n1 == "as"):
			return 1, true // gave a bow then left
		case (possessives[p1] || p1 == "the") && hasArcheryWord(text):
			return 0, true // her bow, in a paragraph about arrows
		}
	}
	return 0, false
}

// pronunciationNarrationPastTense is resolveHomographByGrammar's "narration
// is past tense" assumption for a bare "read" in a non-quote paragraph with
// no other cue.
const pronunciationNarrationPastTense = true

var (
	modals = map[string]bool{
		"to": true, "can": true, "could": true, "will": true, "would": true, "shall": true,
		"should": true, "must": true, "may": true, "might": true, "cannot": true, "can't": true,
		"couldn't": true, "won't": true, "wouldn't": true, "shouldn't": true, "do": true,
		"does": true, "don't": true, "doesn't": true, "didn't": true, "did": true, "let's": true,
		"let": true, "please": true,
	}
	perfectOrPassive = map[string]bool{
		"had": true, "has": true, "have": true, "hadn't": true, "hasn't": true, "haven't": true,
		"been": true, "being": true, "was": true, "were": true, "be": true, "is": true,
		"are": true, "well": true,
	}
	softAdverbs = map[string]bool{
		"not": true, "never": true, "even": true, "also": true, "just": true, "already": true,
		"quite": true, "only": true, "all": true, "barely": true, "ever": true,
	}
	readNounAdjectives = map[string]bool{
		"a": true, "good": true, "quick": true, "easy": true, "grim": true, "great": true,
		"long": true, "light": true, "interesting": true,
	}
	determiners = map[string]bool{
		"a": true, "an": true, "the": true, "that": true, "this": true, "one": true, "each": true,
		"every": true, "her": true, "his": true, "my": true, "your": true, "their": true,
		"our": true, "its": true,
	}
	possessives = map[string]bool{
		"her": true, "his": true, "my": true, "your": true, "their": true, "our": true, "its": true,
	}
	windParticles  = map[string]bool{"up": true, "down": true, "around": true, "back": true, "through": true}
	windObjects    = map[string]bool{"the": true, "it": true, "him": true, "her": true, "them": true, "thee": true, "its": true}
	leadMetalNouns = map[string]bool{
		"pipe": true, "pipes": true, "weight": true, "weights": true, "shot": true,
		"poisoning": true, "soldier": true, "soldiers": true, "lined": true, "ball": true,
		"balls": true, "paint": true,
	}
	closeDegreeWords = map[string]bool{
		"too": true, "so": true, "as": true, "very": true, "how": true, "this": true, "that": true,
		"pretty": true, "quite": true, "fairly": true, "really": true, "real": true, "rather": true,
		"damn": true, "awful": true, "awfully": true, "dangerously": true, "uncomfortably": true,
		"extremely": true, "incredibly": true, "perilously": true, "anywhere": true, "somewhere": true,
		"nowhere": true, "something": true, "anything": true,
	}
	closeNearNext = map[string]bool{
		"to": true, "enough": true, "by": true, "together": true, "friend": true, "friends": true,
		"call": true, "calls": true, "range": true, "quarters": true, "attention": true,
		"proximity": true, "combat": true, "second": true, "eye": true, "watch": true,
		"scrutiny": true, "relative": true, "relatives": true, "family": true, "ally": true,
		"allies": true, "contact": true, "protection": true, "observation": true, "inspection": true,
		"formation": true, "look": true, "shave": true, "match": true, "encounter": true,
	}
	closeLinkingVerbs = map[string]bool{
		"was": true, "were": true, "is": true, "are": true, "am": true, "be": true, "been": true,
		"being": true, "wasn't": true, "weren't": true, "isn't": true, "aren't": true,
		"it's": true, "i'm": true, "we're": true, "you're": true, "they're": true, "he's": true,
		"she's": true, "that's": true, "get": true, "gets": true, "got": true, "getting": true,
		"come": true, "comes": true, "came": true, "coming": true, "stay": true, "stays": true,
		"stayed": true, "staying": true, "stand": true, "stood": true, "standing": true,
		"keep": true, "kept": true, "keeping": true, "remain": true, "remained": true,
		"move": true, "moved": true, "moving": true, "lean": true, "leaned": true, "leaning": true,
		"step": true, "stepped": true, "stepping": true, "draw": true, "drew": true, "drawing": true,
		"grow": true, "grew": true, "grown": true, "become": true, "became": true, "feel": true,
		"felt": true, "seem": true, "seemed": true, "look": true, "looked": true, "sat": true,
		"lay": true, "follow": true, "followed": true, "following": true, "pressed": true,
		"huddled": true, "stick": true, "stuck": true, "sticking": true,
	}
	closeObjects = map[string]bool{
		"the": true, "it": true, "them": true, "her": true, "his": true, "my": true, "your": true,
		"their": true, "our": true, "its": true, "this": true, "that": true, "these": true,
		"those": true,
	}
	closeHoldVerbs = map[string]bool{
		"hold": true, "held": true, "holding": true, "keep": true, "kept": true, "keeping": true,
		"pull": true, "pulled": true, "pulling": true, "draw": true, "drew": true, "drawing": true,
		"hug": true, "hugged": true, "hugging": true, "clutch": true, "clutched": true,
		"clutching": true, "press": true, "pressed": true, "pressing": true, "tug": true, "tugged": true,
	}
	closeHeldPronouns = map[string]bool{"her": true, "him": true, "them": true, "me": true, "it": true, "us": true, "you": true}
	woundAdjectives   = map[string]bool{
		"stab": true, "bullet": true, "knife": true, "sword": true, "arrow": true, "gunshot": true,
		"puncture": true, "head": true, "chest": true, "leg": true, "arm": true, "neck": true,
		"stomach": true, "shoulder": true, "entry": true, "exit": true, "flesh": true,
		"open": true, "deep": true, "bad": true, "fresh": true, "old": true, "new": true,
		"gaping": true, "bleeding": true, "mortal": true, "fatal": true, "grievous": true,
		"nasty": true, "shallow": true, "small": true, "large": true, "terrible": true,
		"ragged": true, "healing": true, "infected": true, "wicked": true, "serious": true,
		"minor": true, "worst": true, "ugly": true, "savage": true, "of": true, "any": true,
		"no": true, "some": true, "another": true, "only": true, "same": true, "single": true,
		"or": true,
	}
	woundParticles = map[string]bool{
		"round": true, "about": true, "along": true, "between": true, "into": true, "past": true,
		"toward": true, "towards": true, "away": true, "upward": true,
		"upwards": true, "downward": true, "onward": true, "tight": true, "tightly": true,
		"itself": true, "themselves": true, "together": true, "among": true, "amongst": true,
		"higher": true, "deeper": true, "from": true,
	}
	woundObjects = map[string]bool{
		"way": true, "arm": true, "arms": true, "fingers": true, "hand": true, "hands": true,
		"tail": true, "legs": true, "body": true, "coils": true, "tentacles": true, "rope": true,
		"hair": true, "scarf": true, "clock": true, "watch": true, "path": true, "course": true,
		"cord": true, "chain": true, "thread": true, "wire": true, "bandage": true, "cloth": true,
	}
	liveNouns = map[string]bool{
		"fire": true, "feed": true, "feeds": true, "round": true, "rounds": true,
		"ammunition": true, "ammo": true, "wire": true, "wires": true, "grenade": true,
		"grenades": true, "bait": true, "music": true, "show": true, "broadcast": true,
		"stream": true, "footage": true, "coverage": true, "video": true, "audio": true,
		"action": true, "animal": true, "animals": true, "prey": true, "target": true,
		"targets": true, "performance": true, "subject": true, "subjects": true,
		"specimen": true, "specimens": true, "test": true, "tests": true, "data": true,
		"view": true, "update": true, "updates": true, "combat": true, "weapon": true,
		"weapons": true, "coals": true, "current": true, "link": true, "connection": true,
		"one": true, "ones": true, "capture": true, "munitions": true, "explosives": true,
		"television": true, "tv": true, "telemetry": true, "tracking": true, "steel": true,
		"blade": true, "blades": true, "training": true, "exercise": true, "demonstration": true,
	}
	liveLinkingVerbs = map[string]bool{
		"go": true, "goes": true, "going": true, "went": true, "gone": true, "was": true,
		"is": true, "are": true, "were": true, "be": true, "been": true, "it's": true,
		"we're": true, "they're": true, "that's": true, "broadcast": true,
		"broadcasting": true, "streamed": true, "streaming": true, "performed": true,
		"performing": true, "captured": true, "taken": true, "caught": true, "eaten": true,
	}
	liveVerbNext = map[string]bool{
		"in": true, "on": true, "at": true, "with": true, "here": true, "there": true,
		"for": true, "through": true, "to": true, "like": true, "long": true, "longer": true,
		"by": true, "forever": true, "alone": true, "together": true, "without": true,
		"among": true, "under": true, "near": true, "nearby": true, "up": true, "off": true,
		"again": true, "anymore": true, "until": true, "out": true, "down": true, "next": true,
		"across": true, "happily": true, "another": true, "and": true, "or": true, "beyond": true,
		"within": true, "inside": true, "outside": true, "above": true, "below": true,
		"into": true, "around": true, "free": true, "underground": true,
	}
	liveSubjects = map[string]bool{
		"who": true, "people": true, "humans": true, "men": true, "women": true, "others": true,
		"things": true, "creatures": true, "folk": true, "all": true, "both": true, "still": true,
		"never": true, "always": true, "only": true, "just": true, "actually": true, "really": true,
		"and": true, "then": true, "ever": true, "long": true, "yet": true, "already": true,
		"now": true, "usually": true, "routinely": true, "that": true, "which": true,
	}
	livesNounBefore = map[string]bool{
		"their": true, "our": true, "your": true, "his": true, "her": true, "my": true, "its": true,
		"the": true, "of": true, "many": true, "more": true, "these": true, "those": true,
		"own": true, "innocent": true, "human": true, "past": true, "whole": true, "entire": true,
		"both": true, "new": true, "normal": true, "daily": true, "long": true, "short": true,
		"other": true, "all": true, "countless": true, "thousand": true, "thousands": true,
		"hundred": true, "hundreds": true, "million": true, "millions": true, "billions": true,
		"two": true, "three": true, "four": true, "five": true, "six": true, "seven": true,
		"eight": true, "nine": true, "ten": true, "few": true, "several": true, "some": true,
		"no": true, "any": true, "save": true, "saved": true, "saving": true, "saves": true,
		"lose": true, "lost": true, "losing": true, "risk": true, "risked": true, "risking": true,
		"cost": true, "costing": true, "claim": true, "claimed": true, "took": true, "take": true,
		"taking": true, "taken": true, "spare": true, "spared": true, "for": true, "with": true,
		"people's": true, "everyone's": true, "private": true, "separate": true, "previous": true,
		"future": true, "former": true, "everyday": true, "mortal": true, "extra": true,
		"enough": true, "what": true, "whose": true, "ruin": true, "ruined": true, "change": true,
		"changed": true, "live": true, "lived": true, "living": true,
	}
	livesVerbBefore = map[string]bool{
		"he": true, "she": true, "it": true, "who": true, "that": true, "which": true,
		"one": true, "nobody": true, "everyone": true, "someone": true, "anyone": true,
		"still": true, "currently": true, "now": true, "never": true, "always": true, "only": true,
		"just": true, "actually": true, "really": true, "also": true, "nothing": true,
		"something": true, "anything": true, "everything": true, "whatever": true,
		"whoever": true,
	}
	minuteTinyNouns = map[string]bool{
		"details": true, "detail": true, "adjustments": true, "adjustment": true, "changes": true,
		"change": true, "amounts": true, "amount": true, "movements": true, "movement": true,
		"differences": true, "difference": true, "particles": true, "cracks": true,
		"fractures": true, "variations": true, "shifts": true, "shift": true, "traces": true,
		"quantities": true, "quantity": true, "fluctuations": true, "scale": true, "size": true,
		"fragments": true, "tremors": true, "vibrations": true, "imperfections": true,
		"flaws": true, "corrections": true, "twitch": true, "twitches": true, "increments": true,
		"degree": true, "degrees": true, "control": true, "precision": true, "scratches": true,
		"portion": true, "portions": true, "pieces": true, "specks": true, "examination": true,
		"inspection": true, "attention": true, "observations": true, "threads": true, "gaps": true,
		"motions": true, "motion": true, "signs": true, "alterations": true, "flecks": true,
		"pores": true, "fraction": true, "fractions": true, "runes": true, "structures": true,
		"points": true, "holes": true, "droplets": true, "sparks": true, "tendrils": true,
		"filaments": true, "strands": true, "manipulations": true, "increase": true,
		"improvement": true, "improvements": true, "tweaks": true, "ripples": true, "pulses": true,
	}
	tearsDropBefore = map[string]bool{
		"in": true, "of": true, "with": true, "into": true, "to": true, "the": true, "her": true,
		"his": true, "my": true, "your": true, "their": true, "our": true, "these": true,
		"those": true, "by": true, "through": true, "back": true, "hot": true, "fresh": true,
		"angry": true, "bitter": true, "happy": true, "unshed": true, "sudden": true, "more": true,
		"no": true, "any": true, "few": true, "crocodile": true, "and": true, "silent": true,
		"own": true, "shed": true, "sheds": true, "from": true, "as": true, "near": true,
		"were": true, "wiped": true, "wiping": true, "blinked": true, "blinking": true,
		"fought": true, "fighting": true, "cried": true, "wept": true, "shedding": true,
	}
	tearsRipNext = map[string]bool{
		"through": true, "into": true, "apart": true, "open": true, "free": true, "loose": true,
		"itself": true, "asunder": true,
	}
	tearsRipAfterSubject = map[string]bool{"off": true, "away": true, "out": true, "at": true, "down": true, "up": true}
	tearsSubjects        = map[string]bool{
		"that": true, "which": true, "who": true, "something": true, "then": true, "and": true,
	}
	tearsRipFabricRE     = regexp.MustCompile(`^ in (?:(?:the|his|her|its|their|my|your) )?(?:fabric|cloth|clothes|clothing|shirt|sleeve|sleeves|armor|armour|canvas|skin|flesh|hull|sail|sails|wall|walls|veil|reality|space|uniform|coat|cloak|jacket|dress|robe|robes|pants|trousers|suit|wing|wings|tent|net|page|paper|muscle|muscles|leather|hide|world)\b`)
	bowGestureVerbs      = map[string]bool{"scrape": true, "kneel": true, "smile": true, "nod": true, "curtsy": true}
	bowGestureAdjectives = map[string]bool{
		"small": true, "slight": true, "deep": true, "low": true, "formal": true, "polite": true,
		"quick": true, "curt": true, "shallow": true, "courtly": true, "gracious": true,
		"mocking": true, "stiff": true, "little": true, "half": true, "respectful": true,
		"sweeping": true, "elaborate": true, "exaggerated": true,
	}
	bowWeaponAdjectives = map[string]bool{
		"hunting": true, "short": true, "long": true, "compound": true, "recurve": true,
		"magical": true, "unstrung": true, "strongest": true,
	}
	bowWeaponNext = map[string]bool{
		"drawn": true, "bent": true, "string": true, "strung": true, "bending": true,
		"training": true, "arm": true, "hand": true, "held": true,
	}
)

// isModal also accepts any "'ll" contraction ("I'll read", "she'll lead").
func isModal(w string) bool {
	return modals[w] || strings.HasSuffix(w, "'ll")
}

func isSubjectPronoun(w string) bool {
	switch w {
	case "i", "you", "he", "she", "we", "they", "it":
		return true
	}
	return false
}

// capitalizedBefore reports whether the word directly before byte offset
// i in text starts with a capital and isn't the first word of its
// sentence - most often a name.
func capitalizedBefore(text string, i int) bool {
	before := strings.TrimRight(text[:i], " ")
	j := strings.LastIndexAny(before, " \n")
	if j < 0 {
		return false
	}
	lead := strings.TrimRight(before[:j], " ")
	if lead == "" || strings.ContainsAny(lead[len(lead)-1:], ".!?") || strings.HasSuffix(lead, "“") || strings.HasSuffix(lead, "\"") {
		return false
	}
	return isCapitalized(before[j+1:])
}

func isCapitalized(w string) bool {
	for _, r := range w {
		return unicode.IsUpper(r)
	}
	return false
}

// hasArcheryWord reports whether text talks about archery - enough to read
// a possessive "her bow" as the weapon rather than a returned gesture.
func hasArcheryWord(text string) bool {
	lower := strings.ToLower(text)
	for _, w := range []string{"arrow", "quiver", "nock", "loose", "shoot", "shot", "aim", "bowstring"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// wordsBefore returns the n words immediately before byte offset i in text,
// oldest first, padded with "" at the front - lowercased, with curly
// apostrophes folded to straight ones. Stops at sentence punctuation or an
// opening quote, so "...said. Read it" sees no words before "Read".
func wordsBefore(text string, i, n int) []string {
	out := make([]string, n)
	fields := tokenWords(text[:i], true)
	for k := 0; k < n && k < len(fields); k++ {
		out[n-1-k] = fields[len(fields)-1-k]
	}
	return out
}

// nextWords returns the first two words of text, normalized the same way as
// wordsBefore, stopping at clause punctuation.
func nextWords(text string) (string, string) {
	fields := tokenWords(text, false)
	var a, b string
	if len(fields) > 0 {
		a = fields[0]
	}
	if len(fields) > 1 {
		b = fields[1]
	}
	return a, b
}

// tokenWords splits text into normalized words, keeping only the run
// nearest the term: the part after the last clause break when fromEnd, or
// before the first one otherwise.
func tokenWords(text string, fromEnd bool) []string {
	const breaks = ".,!?;:\"“”\n"
	if fromEnd {
		if j := strings.LastIndexAny(text, breaks); j >= 0 {
			text = text[j:]
		}
	} else if j := strings.IndexAny(text, breaks); j >= 0 {
		text = text[:j]
	}
	text = strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(text))
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != '-'
	})
	out := words[:0]
	for _, w := range words {
		if w = strings.Trim(w, "-'"); w != "" {
			out = append(out, w)
		}
	}
	return out
}
