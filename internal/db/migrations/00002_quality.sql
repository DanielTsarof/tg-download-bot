-- +goose Up
ALTER TABLE requests ADD COLUMN quality TEXT NOT NULL DEFAULT ''; -- '' for audio

-- Videos cached so far have an unknown resolution; let them be downloaded again.
DELETE FROM file_cache WHERE format = 'mp4';
ALTER TABLE file_cache ADD COLUMN quality TEXT NOT NULL DEFAULT '';
ALTER TABLE file_cache DROP CONSTRAINT file_cache_pkey;
ALTER TABLE file_cache ADD PRIMARY KEY (lookup_key, format, quality);

-- +goose Down
DELETE FROM file_cache WHERE quality <> '';
ALTER TABLE file_cache DROP CONSTRAINT file_cache_pkey;
ALTER TABLE file_cache DROP COLUMN quality;
ALTER TABLE file_cache ADD PRIMARY KEY (lookup_key, format);
ALTER TABLE requests DROP COLUMN quality;
