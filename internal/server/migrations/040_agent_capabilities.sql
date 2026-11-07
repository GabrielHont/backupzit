-- Explicit feature negotiation avoids inferring support from upstream versions.
ALTER TABLE agents ADD COLUMN capabilities TEXT[] NOT NULL DEFAULT '{}';
