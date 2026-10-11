-- The baseline global world facts: how casting voices a kind of being
-- that has no signal of its own - the genre-standard accents
-- speakerattr's characterizeSystemPrompt lists, more kinds beside them,
-- which kinds are long- or short-lived or don't age, and a few rules that
-- hold across books (nobles, pirates, mind speech) - as facts the reader
-- can see, edit, and override per series. Upserts: a library that already
-- has a global fact under one of these keys gets this wording and these
-- aliases, and keeps the fact's id and status.

-- +goose Up
INSERT INTO world_facts (id, scope, key_norm, key, aliases, fact, source, status, created_at, updated_at)
SELECT lower(hex(randomblob(16))), 'global', column1, column1, column2, column3, 'manual', 'active',
       CAST(strftime('%s', 'now') AS INTEGER) * 1000, CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM (VALUES
    ('dwarf', '["dwarven", "dwarves", "dwarvish"]', 'With no accent signal of their own, a Scottish accent. Long-lived: decades or centuries of age are not an old voice.'),
    ('elf', '["elven", "elves", "elvish"]', 'With no accent signal of their own, refined Received Pronunciation. Long-lived: decades or centuries of age are not an old voice.'),
    ('orc', '["orcish"]', 'With no accent signal of their own, a guttural Cockney accent.'),
    ('goblin', '[]', 'With no accent signal of their own, a guttural Cockney accent. Short-lived: adult within a few years, and middle-aged in their teens.'),
    ('giant', '[]', 'One of the giant kind, with no accent signal of their own, has a broad rural accent.'),
    ('troll', '[]', 'With no accent signal of their own, a broad rural accent.'),
    ('dragon', '["draconic"]', 'With no accent signal of their own, plummy aristocratic Received Pronunciation. Long-lived: decades or centuries of age are not an old voice.'),
    ('demon', '["demonic"]', 'With no accent signal of their own, plummy aristocratic Received Pronunciation; a beastly demon growls instead.'),
    ('fairy', '["fairies", "faerie", "fae"]', 'With no accent signal of their own, a light Irish accent.'),
    ('robot', '["AI", "construct", "android"]', 'With no voice signal of their own, flat and precisely enunciated, with no accent. Does not age: years passing change nothing about the voice.'),
    ('ghost', '[]', 'With no voice signal of their own, faint, old-fashioned English. Does not age: years passing change nothing about the voice.'),
    ('kobold', '[]', 'A small reptilian kind, vaguely draconic: with no voice signal of their own, a thin, raspy voice with a faint hiss, and the same accent as the rest of the cast. Short-lived: adult within a few years, and middle-aged in their teens.'),
    ('vampire', '["vampiric"]', 'With no voice signal of their own, smooth, unhurried and old-world formal, at a human pitch. Long-lived: decades or centuries of age are not an old voice.'),
    ('werewolf', '["werewolves", "beastkin", "beastfolk", "lycanthrope"]', 'With no voice signal of their own, rough and low, with a growl under the words, in the same accent as the rest of the cast.'),
    ('lizardfolk', '["lizardman", "lizardmen", "naga", "snakefolk", "serpentfolk"]', 'With no voice signal of their own, sibilant and dry, with drawn-out s sounds.'),
    ('gnome', '[]', 'With no voice signal of their own, light, quick and bright.'),
    ('halfling', '["hobbit"]', 'With no voice signal of their own, light and bright, with a West Country accent.'),
    ('ogre', '[]', 'With no voice signal of their own, slow and very low, with simple phrasing.'),
    ('undead', '["lich", "liches", "skeleton", "zombie", "revenant"]', 'With no voice signal of their own, dry, hollow and breathless. Does not age: years passing change nothing about the voice.'),
    ('angel', '["angelic", "celestial", "seraph"]', 'With no voice signal of their own, clear, resonant and calm.'),
    ('elemental', '[]', 'With no voice signal of their own, airy and soft for one of air or water, deep and grinding for one of earth or stone.'),
    ('spirit', '["dryad", "nymph"]', 'One of the spirit kind, with no voice signal of their own, is airy and soft.'),
    ('dungeon core', '["system voice", "system announcement", "status announcer"]', 'With no voice signal of their own, neutral and even, with no emotion and no accent. Does not age: years passing change nothing about the voice.'),
    ('insectoid', '["hive mind", "hivemind"]', 'With no voice signal of their own, clipped and clicking, with buzzing consonants.'),
    ('merfolk', '["mermaid", "merman", "mermen", "siren"]', 'With no voice signal of their own, liquid, melodic and unhurried.'),
    ('royalty', '["noble", "nobleman", "noblewoman", "king", "queen", "prince", "princess", "duke", "duchess"]', 'A more formal register, never a different accent from their people.'),
    ('pirate', '["sailor", "buccaneer", "corsair"]', 'With no accent signal of their own, in a seafaring story, a West Country accent.'),
    ('telepathy', '["telepathic", "telepathically", "mind speech", "mindspeech"]', 'Speech sent mind to mind is in the speaker’s own voice, with no added effect.')
)
WHERE true
ON CONFLICT (scope, key_norm) DO UPDATE SET
    aliases = excluded.aliases,
    fact = excluded.fact,
    updated_at = excluded.updated_at;
