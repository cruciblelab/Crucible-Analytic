//go:build integration

// How much of a table a backup of it actually is.
//
// # Why this is a test and not a paragraph
//
// Because it was a paragraph, and the paragraph was wrong.
//
// estimate.go carried two measured numbers and a conclusion that did not
// follow from them: rows that were near-identical compressed to half a
// per cent, and the comment reasoned from that to "the estimate assumes a
// fifth, which is forty times worse than measured". What had been
// measured was gzip against repetition. A customer's rows are not
// repetitive in the column that dominates the file - ip_hash is 32 bytes
// of SHA-256 on every row, and nothing compresses it.
//
// A sentence cannot go red. This can: three kinds of row, the real
// Measure, the real writer, and the ratio each one produces.
//
// *Bir gerekçenin ölçülmüş bir sayı içermesi, o sayının gerekçeyi
// desteklediği anlamına gelmez.*

package backup_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
)

// ratioRows is how many traffic rows each arm writes, with half as many
// beacon rows.
//
// Fifty thousand because that is where the ratio stops moving. Measured
// at three sizes: 5 000 gives 1/66, 1/10, 1/3 and 200 000 gives 1/60,
// 1/9, 1/3 - a small table is proportionally more page and index
// overhead, so it flatters the ratio, and the flattery is gone by here.
const ratioRows = 50000

// TestTheEstimateIsNotOptimisticAboutAnyKindOfRow.
func TestTheEstimateIsNotOptimisticAboutAnyKindOfRow(t *testing.T) {
	// Bands rather than exact numbers: the compressed size moves by a
	// few bytes between runs because the timestamps do. Wide enough not
	// to flake, narrow enough that a schema change which alters what a
	// backup costs lands outside one.
	for _, arm := range []struct {
		name            string
		fill            func(*testing.T, *pgxpool.Pool, int)
		atLeast, atMost float64
		// overBy is how many times the estimate may exceed the file that
		// actually lands, or zero for arms where it is not checked.
		//
		// Only the realistic arm carries one, because only the realistic
		// arm is what the number on the page is about. The estimate is
		// allowed to be three times the truth there by construction -
		// the share is 1/3 and this data is 1/9 - and a fourth is the
		// room a schema change may take before somebody should look.
		//
		// It is here because a mutation walked past everything else:
		// setting the estimate to the tables' whole size passed, since
		// the checks below all measure what the *file* costs and none of
		// them measured whether the *estimate* was any use. An estimate
		// that refuses every backup is wrong in the polite direction and
		// still wrong.
		overBy float64
		what   string
	}{
		{
			name: "tekrarli", fill: fillRepetitive,
			atLeast: 0.010, atMost: 0.030,
			what: "near-identical rows, which is what the old comment measured " +
				"and reasoned from",
		},
		{
			name: "gercekci", fill: fillRealistic,
			atLeast: 0.080, atMost: 0.140, overBy: 4,
			what: "a distinct pseudonym and visitor id on every row with the " +
				"descriptive columns from a small pool, which is what a " +
				"deployment produces",
		},
		{
			name: "kotucul", fill: fillAdversarial,
			atLeast: 0.250, atMost: 0.333,
			what: "every text column high-entropy as well, which nothing produces",
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			ctx := context.Background()
			pool := scratchDatabase(t, "ca_ratio_"+arm.name)
			arm.fill(t, pool, ratioRows)

			dir := t.TempDir()
			est, err := backup.Measure(ctx, pool, dir, []string{backup.SetAnalitik})
			if err != nil {
				t.Fatal(err)
			}
			w := backup.Writer{
				Pool: pool, Dir: dir,
				BinaryVersion: "v0.0.0-test", SchemaVersion: 99,
			}
			res, err := w.Write(ctx, "olcum.tar.gz", []string{backup.SetAnalitik})
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(res.Path)
			if err != nil {
				t.Fatal(err)
			}

			ratio := float64(info.Size()) / float64(est.TableBytes)
			t.Logf("%s: %d bytes on disk, %d bytes of file, ratio 1/%.0f",
				arm.name, est.TableBytes, info.Size(), 1/ratio)

			// The one that ties the measurement to the constant. An
			// estimate below the truth is the shape that fills a disk.
			if info.Size() > est.FileBytes {
				t.Errorf("the estimate promised at most %d bytes and the file is "+
					"%d - %s.\n"+
					"CompressedShare is 1/%d and this arm needs 1/%.1f. The "+
					"estimate is now optimistic, which means the only thing "+
					"between a customer and a full disk is spaceGuard",
					est.FileBytes, info.Size(), arm.what,
					backup.CompressedShare, 1/ratio)
			}

			if arm.overBy > 0 && float64(est.FileBytes) > float64(info.Size())*arm.overBy {
				t.Errorf("the estimate promised %d bytes and the file is %d, which "+
					"is %.1f times over on the data this number is for.\n"+
					"An estimate nobody can be surprised by is one that refuses "+
					"backups which would have fitted, and the customer's only "+
					"way past it is a bigger disk",
					est.FileBytes, info.Size(),
					float64(est.FileBytes)/float64(info.Size()))
			}

			if ratio > arm.atMost {
				t.Errorf("this arm compresses to %.3f of the tables, above the "+
					"%.3f measured for it (%s). Something made a backup more "+
					"expensive than it was", ratio, arm.atMost, arm.what)
			}
			if ratio < arm.atLeast {
				t.Errorf("this arm compresses to %.3f of the tables, below the "+
					"%.3f measured for it (%s). The arm has stopped being the "+
					"kind of data it is named for, so the band above it is no "+
					"longer evidence of anything", ratio, arm.atLeast, arm.what)
			}
		})
	}
}

// fillRepetitive is the shape the old comment measured: rows that differ
// only in their timestamp.
func fillRepetitive(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO traffic_snapshots
		    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		     request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org,
		     is_known_bot_asn, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'site',
		       ('203.0.113.' || (g % 256))::inet,
		       't13d1516h2_8daaf6152771_b186095e22b6',
		       0, 0, 0, 0, false, 'TR', 0, '', false,
		       sha256('sabit'::bytea)
		FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO beacon_events
		    (time, site_id, visitor_id, event_type, path, ip, browser, os,
		     device, language, country, asn, asn_org, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'site', 'v', 'pageview',
		       '/', ('203.0.113.' || (g % 256))::inet, 'Chrome', 'Windows',
		       'desktop', 'tr', 'TR', 0, '', sha256('sabit'::bytea)
		FROM generate_series(1, $1) AS g`, n/2); err != nil {
		t.Fatal(err)
	}
}

// fillRealistic is what a deployment produces: a distinct pseudonym on
// every row, because ip_hash is a SHA-256 of the address and the key,
// and a distinct visitor id every few rows - with the descriptive
// columns drawn from a small pool, because a shop has a handful of
// pages, browsers and countries.
func fillRealistic(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO traffic_snapshots
		    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		     request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org,
		     is_known_bot_asn, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'magaza',
		       ('198.51.' || (g % 256) || '.' || ((g / 256) % 256))::inet,
		       (ARRAY['t13d1516h2_8daaf6152771_b186095e22b6',
		              't13d1517h2_8daaf6152771_02713d6af862',
		              't13d3112h2_e8f1e7e78f70_5ac7a67f21f5'])[1 + g % 3],
		       g % 40, g % 60, (g % 400) / 10.0, g % 100, g % 17 = 0,
		       (ARRAY['TR','DE','US','NL','FR','GB'])[1 + g % 6],
		       (ARRAY[9121,3320,15169,16509,8075,42926])[1 + g % 6],
		       (ARRAY['Turk Telekom','Deutsche Telekom AG','GOOGLE',
		              'AMAZON-02','MICROSOFT-CORP','Rackspace'])[1 + g % 6],
		       g % 23 = 0, sha256(('ziyaretci-' || g)::bytea)
		FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO beacon_events
		    (time, site_id, visitor_id, event_type, event_name, path, query,
		     title, utm_source, utm_medium, utm_campaign, ref, referrer_host,
		     referrer_path, ip, browser, os, device, is_bot_ua, screen_w,
		     screen_h, language, country, asn, asn_org, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'magaza',
		       encode(sha256(('v-' || g / 7)::bytea), 'hex'),
		       (ARRAY['pageview','event','pageview','pageview'])[1 + g % 4],
		       (ARRAY['','sepete_ekle','','odeme'])[1 + g % 4],
		       (ARRAY['/','/urunler','/urunler/ayakkabi','/sepet','/iletisim',
		              '/blog/yeni-sezon'])[1 + g % 6],
		       (ARRAY['','?sayfa=2','?renk=siyah&beden=42',''])[1 + g % 4],
		       (ARRAY['Ana sayfa','Ürünler','Ayakkabı','Sepet'])[1 + g % 4],
		       (ARRAY['google','','instagram','newsletter'])[1 + g % 4],
		       (ARRAY['organic','','cpc','email'])[1 + g % 4],
		       (ARRAY['','yaz-indirimi','','kara-cuma'])[1 + g % 4],
		       '', (ARRAY['google.com','','instagram.com','t.co'])[1 + g % 4],
		       (ARRAY['/','/search',''])[1 + g % 3],
		       ('198.51.' || (g % 256) || '.' || ((g / 256) % 256))::inet,
		       (ARRAY['Chrome','Safari','Firefox','Edge'])[1 + g % 4],
		       (ARRAY['Windows','macOS','Android','iOS'])[1 + g % 4],
		       (ARRAY['desktop','mobile','mobile','tablet'])[1 + g % 4],
		       g % 31 = 0, (ARRAY[1920,1440,390,412])[1 + g % 4],
		       (ARRAY[1080,900,844,915])[1 + g % 4],
		       (ARRAY['tr','tr-TR','en-US','de'])[1 + g % 4],
		       (ARRAY['TR','DE','US','NL'])[1 + g % 4],
		       (ARRAY[9121,3320,15169,16509])[1 + g % 4],
		       (ARRAY['Turk Telekom','Deutsche Telekom AG','GOOGLE',
		              'AMAZON-02'])[1 + g % 4],
		       sha256(('ziyaretci-' || g)::bytea)
		FROM generate_series(1, $1) AS g`, n/2); err != nil {
		t.Fatal(err)
	}
}

// fillAdversarial makes every text column high-entropy too.
//
// Nothing produces this, and it is here for one reason: it is the arm
// that broke the old constant. One fifth held for the two arms above and
// not for this one, and the guard exists because "nothing produces this"
// is a claim about today's data.
func fillAdversarial(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO traffic_snapshots
		    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		     request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org,
		     is_known_bot_asn, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'site',
		       ('198.51.' || (g % 256) || '.' || ((g / 256) % 256))::inet,
		       encode(sha256((g || 'a')::bytea), 'hex'),
		       g, g, g / 3.0, g % 100, false,
		       upper(substr(encode(sha256((g || 'b')::bytea), 'hex'), 1, 2)),
		       g, encode(sha256((g || 'c')::bytea), 'hex'), false,
		       sha256((g || 'd')::bytea)
		FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO beacon_events
		    (time, site_id, visitor_id, event_type, path, query, title,
		     ip, browser, os, device, language, country, asn, asn_org, ip_hash)
		SELECT now() - (g || ' seconds')::interval, 'site',
		       encode(sha256((g || 'v')::bytea), 'hex'), 'pageview',
		       '/' || encode(sha256((g || 'p')::bytea), 'hex'),
		       encode(sha256((g || 'q')::bytea), 'hex'),
		       encode(sha256((g || 't')::bytea), 'hex'),
		       ('198.51.' || (g % 256) || '.' || ((g / 256) % 256))::inet,
		       encode(sha256((g || 'br')::bytea), 'hex'),
		       encode(sha256((g || 'os')::bytea), 'hex'),
		       encode(sha256((g || 'de')::bytea), 'hex'),
		       encode(sha256((g || 'la')::bytea), 'hex'),
		       upper(substr(encode(sha256((g || 'co')::bytea), 'hex'), 1, 2)),
		       g, encode(sha256((g || 'ao')::bytea), 'hex'),
		       sha256((g || 'ih')::bytea)
		FROM generate_series(1, $1) AS g`, n/2); err != nil {
		t.Fatal(err)
	}
}
