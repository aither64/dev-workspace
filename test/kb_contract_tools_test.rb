# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'minitest/autorun'
require 'open3'
require 'tmpdir'
require 'yaml'
require_relative '../lib/kb_release'

class KbContractToolsTest < Minitest::Test
  ROOT = File.expand_path('..', __dir__)
  BUILD = File.join(ROOT, 'bin/kb-contract-build')
  FETCH = File.join(ROOT, 'bin/kb-contract-fetch')
  MANIFEST = File.join(ROOT, 'bin/kb-contract-manifest')
  RECONCILE = File.join(ROOT, 'bin/kb-contract-reconcile')

  def test_fetch_command_has_a_stable_cli
    output, error, status = Open3.capture3(FETCH, '--help')

    assert(status.success?, error)
    assert_match(/kb-contract-fetch --output DIR/, output)
  end

  def test_builds_candidates_and_paired_manifests
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      plan = write_plan(dir)

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/2 changed pages with 2 annotations/, output)

      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      assert_equal(2, index.fetch('pages').count { |page| page.fetch('changed') })
      assert_includes(
        File.read(File.join(candidate, 'cs/navody/test.txt')),
        '<vpsadmin-nav id="member.edit-profile.open">Upravit profil</vpsadmin-nav>'
      )

      cs_manifest = File.join(dir, 'kb-release-cs.yml')
      en_manifest = File.join(dir, 'kb-release-en.yml')
      run_manifest(source, candidate, 'cs', 'Český souhrn změny', cs_manifest)
      run_manifest(source, candidate, 'en', 'English change summary', en_manifest)

      cs = YAML.safe_load_file(cs_manifest)
      en = YAML.safe_load_file(en_manifest)
      assert_equal('cz', cs.fetch('wiki'))
      assert_equal('org', en.fetch('wiki'))
      assert_equal('navody:test', en.fetch('pages').first.fetch('language_counterpart'))
      assert_equal('kb-candidates/en/manuals/test.txt', en.fetch('pages').first.fetch('file'))
    end
  end

  def test_manifest_builds_schema_four_with_per_page_summaries_and_deletions
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      add_source_page(source, 'cs', 'navody:old', "Old guide.\n", revision: '456')
      build_candidate(source, write_plan(dir), candidate)
      changes = write_changes(
        dir,
        [
          {
            'language' => 'cs', 'id' => 'navody:test', 'action' => 'write',
            'summary' => 'Aktualizace testovacího návodu'
          },
          {
            'language' => 'en', 'id' => 'manuals:test', 'action' => 'write',
            'summary' => 'Update the test guide'
          },
          {
            'language' => 'cs', 'id' => 'navody:old', 'action' => 'delete',
            'summary' => 'Odstranění starého návodu'
          }
        ]
      )

      cs_path = File.join(dir, 'kb-release-cs.yml')
      en_path = File.join(dir, 'kb-release-en.yml')
      run_changes_manifest(source, candidate, 'cs', changes, cs_path)
      run_changes_manifest(source, candidate, 'en', changes, en_path)

      cs = YAML.safe_load_file(cs_path)
      en = YAML.safe_load_file(en_path)
      assert_equal(4, cs.fetch('schema'))
      refute(cs.key?('production_summary'))
      assert_equal('Aktualizace testovacího návodu', cs.fetch('pages').first.fetch('summary'))
      assert_equal(
        {
          'id' => 'navody:old',
          'source_revision' => '456',
          'source_sha256' => Digest::SHA256.hexdigest("Old guide.\n"),
          'summary' => 'Odstranění starého návodu'
        },
        cs.fetch('deletions').first
      )
      assert_empty(en.fetch('deletions'))
      assert_equal('Update the test guide', en.fetch('pages').first.fetch('summary'))
    end
  end

  def test_schema_four_changes_must_cover_every_candidate_write
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      build_candidate(source, write_plan(dir), candidate)
      changes = write_changes(
        dir,
        [{
          'language' => 'cs', 'id' => 'navody:test', 'action' => 'write',
          'summary' => 'Aktualizace testovacího návodu'
        }]
      )

      _output, error, status = Open3.capture3(
        MANIFEST,
        '--source', source,
        '--candidate', candidate,
        '--language', 'cs',
        '--changes', changes,
        '--output', File.join(dir, 'release.yml')
      )

      refute(status.success?)
      assert_match(/missing page writes: en:manuals:test/, error)
    end
  end

  def test_schema_four_changes_reject_invalid_deletions_and_summaries
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      build_candidate(source, write_plan(dir), candidate)
      entries = [
        {
          'language' => 'cs', 'id' => 'navody:test', 'action' => 'write',
          'summary' => 'Aktualizace testovacího návodu'
        },
        {
          'language' => 'en', 'id' => 'manuals:test', 'action' => 'write',
          'summary' => 'Update the test guide'
        },
        {
          'language' => 'cs', 'id' => 'navody:missing', 'action' => 'delete',
          'summary' => 'Odstranění starého návodu'
        }
      ]
      changes = write_changes(dir, entries)

      _output, error, status = Open3.capture3(
        MANIFEST,
        '--source', source,
        '--candidate', candidate,
        '--language', 'cs',
        '--changes', changes,
        '--output', File.join(dir, 'release.yml')
      )
      refute(status.success?)
      assert_match(/deletion source page is not inventoried/, error)

      entries.last['id'] = 'navody:old'
      entries.last['summary'] = "first\nsecond"
      add_source_page(source, 'cs', 'navody:old', "Old guide.\n", revision: '456')
      changes = write_changes(dir, entries)
      _output, error, status = Open3.capture3(
        MANIFEST,
        '--source', source,
        '--candidate', candidate,
        '--language', 'cs',
        '--changes', changes,
        '--output', File.join(dir, 'release.yml')
      )
      refute(status.success?)
      assert_match(/summary must be a non-empty single line/, error)
    end
  end

  def test_build_rejects_source_count_drift
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      plan = write_plan(dir)
      data = YAML.safe_load_file(plan)
      data.fetch('replacements').first['count'] = 2
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/expected 2 matches, found 1/, error)
    end
  end

  def test_build_applies_guarded_content_replacements
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      plan = write_plan(dir)
      data = YAML.safe_load_file(plan)
      data['schema'] = 3
      data['content_replacements'] = [
        {
          'language' => 'cs',
          'page' => 'navody:test',
          'before' => "Použij Upravit profil.\n",
          'replacement' => "Použij Upravit profil.\nPřečti si Notifikace.\n"
        }
      ]
      File.write(plan, YAML.dump(data))

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/1 content replacements/, output)

      page = File.read(File.join(candidate, 'cs/navody/test.txt'))
      assert_includes(
        page,
        "Použij <vpsadmin-nav id=\"member.edit-profile.open\">Upravit profil</vpsadmin-nav>.\n" \
        "Přečti si Notifikace.\n"
      )
      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      replacement = index.fetch('content_replacements').fetch(0)
      assert_equal('navody:test', replacement.fetch('page'))
      assert_equal(1, replacement.fetch('count'))
      assert_includes(
        File.read(File.join(candidate, 'review.md')),
        '## Guarded content replacements'
      )
    end
  end

  def test_build_rejects_content_replacement_count_drift
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      plan = write_plan(dir)
      data = YAML.safe_load_file(plan)
      data['schema'] = 3
      data['content_replacements'] = [
        {
          'language' => 'cs',
          'page' => 'navody:test',
          'before' => 'Text that is not present',
          'replacement' => 'Replacement'
        }
      ]
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/content replacement expected 1 matches, found 0/, error)
    end
  end

  def test_builds_guarded_new_pages_and_selected_capture_media
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      captures = File.join(dir, 'captures')
      write_sources(source)
      add_missing_source(source, 'cs', 'navody:notifikace')
      add_missing_source(source, 'en', 'manuals:notifications')
      write_capture_fixture(captures)
      plan = File.join(dir, 'plan.yml')
      File.write(
        plan,
        YAML.dump(
          'schema' => 2,
          'replacements' => [],
          'new_pages' => [
            {
              'language' => 'cs',
              'page' => 'navody:notifikace',
              'body' => "<page>manuals:notifications</page>\n====== Notifikace ======\n"
            },
            {
              'language' => 'en',
              'page' => 'manuals:notifications',
              'body' => "====== Notifications ======\n"
            }
          ],
          'media' => %w[cs en].map do |language|
            { 'language' => language, 'capture' => 'notifications/routes' }
          end,
          'exceptions' => []
        )
      )

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--captures', captures,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/2 changed pages with 0 annotations and 2 media objects/, output)

      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      assert_equal(%w[create create], index.fetch('pages').filter_map do |page|
        page.fetch('policy') if page.fetch('changed')
      end)
      assert_equal(2, index.fetch('media').length)
      assert_equal(
        'cs:screenshots:vpsadmin:notifications:routes.png',
        index.fetch('media').first.fetch('id')
      )

      cs_manifest = File.join(dir, 'kb-release-cs.yml')
      en_manifest = File.join(dir, 'kb-release-en.yml')
      run_manifest(source, candidate, 'cs', 'Přidat návod k notifikacím', cs_manifest)
      run_manifest(source, candidate, 'en', 'Add notifications guide', en_manifest)
      cs = YAML.safe_load_file(cs_manifest)
      en = YAML.safe_load_file(en_manifest)
      assert_equal(3, cs.fetch('schema'))
      assert_equal(1, cs.fetch('pages').length)
      assert_equal('create', cs.fetch('pages').first.fetch('policy'))
      refute(cs.fetch('pages').first.key?('source_revision'))
      assert_equal('create', cs.fetch('media').first.fetch('policy'))
      assert_equal('navody:notifikace', en.fetch('pages').first.fetch('language_counterpart'))
    end
  end

  def test_builds_guarded_capture_media_updates
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      captures = File.join(dir, 'captures')
      write_sources(source)
      write_capture_fixture(captures)
      source_sha256 = Digest::SHA256.hexdigest('existing production media')
      plan = File.join(dir, 'plan.yml')
      File.write(
        plan,
        YAML.dump(
          'schema' => 2,
          'replacements' => [],
          'new_pages' => [],
          'media' => [
            {
              'language' => 'cs',
              'capture' => 'notifications/routes',
              'policy' => 'update',
              'source_sha256' => source_sha256
            }
          ],
          'exceptions' => []
        )
      )

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--captures', captures,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/1 media objects/, output)

      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      media = index.fetch('media').first
      assert_equal('update', media.fetch('policy'))
      assert_equal(source_sha256, media.fetch('source_sha256'))

      manifest_path = File.join(dir, 'kb-release-cs.yml')
      run_manifest(source, candidate, 'cs', 'Aktualizovat snímek', manifest_path)
      manifest_media = YAML.safe_load_file(manifest_path).fetch('media').first
      assert_equal('update', manifest_media.fetch('policy'))
      assert_equal(source_sha256, manifest_media.fetch('source_sha256'))
    end
  end

  def test_build_rejects_unguarded_capture_media_updates
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      captures = File.join(dir, 'captures')
      write_sources(source)
      write_capture_fixture(captures)
      plan = write_media_plan(dir, 'notifications/routes')
      data = YAML.safe_load_file(plan)
      data.fetch('media').first['policy'] = 'update'
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--captures', captures,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/source_sha256 must be a SHA-256 digest/, error)
    end
  end

  def test_build_injects_canonical_code_samples
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      add_missing_source(source, 'cs', 'navody:notifikace')
      FileUtils.mkdir_p(code_root)
      File.write(File.join(code_root, 'server.py'), "print('verified')\n")
      plan = File.join(dir, 'plan.yml')
      File.write(
        plan,
        YAML.dump(
          'schema' => 3,
          'replacements' => [],
          'new_pages' => [
            {
              'language' => 'cs',
              'page' => 'navody:notifikace',
              'body' => "====== Notifikace ======\n\n<kb-code-sample id=\"notifications.webhook-server\" />\n"
            }
          ],
          'code_samples' => [
            {
              'id' => 'notifications.webhook-server',
              'file' => 'server.py',
              'language' => 'python'
            }
          ],
          'media' => [],
          'exceptions' => []
        )
      )

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/1 changed pages/, output)

      page = File.read(File.join(candidate, 'cs/navody/notifikace.txt'))
      assert_includes(page, "<code python>\nprint('verified')\n</code>")
      refute_includes(page, 'kb-code-sample')

      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      assert_equal(1, index.fetch('code_samples').length)
      sample = index.fetch('code_samples').fetch(0)
      assert_equal('notifications.webhook-server', sample.fetch('id'))
      assert_equal(Digest::SHA256.hexdigest("print('verified')\n"), sample.fetch('sha256'))
      assert_includes(
        File.read(File.join(candidate, 'review.md')),
        '`notifications.webhook-server`'
      )
    end
  end

  def test_build_rejects_unused_code_samples
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      FileUtils.mkdir_p(code_root)
      File.write(File.join(code_root, 'server.py'), "print('unused')\n")
      plan = write_plan(dir)
      data = YAML.safe_load_file(plan)
      data['schema'] = 3
      data['code_samples'] = [
        {
          'id' => 'notifications.webhook-server',
          'file' => 'server.py',
          'language' => 'python'
        }
      ]
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/unused code samples: notifications.webhook-server/, error)
    end
  end

  def test_build_rejects_physically_wrapped_new_page_list_items
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      add_missing_source(source, 'en', 'manuals:notifications')
      plan = File.join(dir, 'plan.yml')
      File.write(
        plan,
        YAML.dump(
          'schema' => 3,
          'replacements' => [],
          'new_pages' => [
            {
              'language' => 'en',
              'page' => 'manuals:notifications',
              'body' => "====== Notifications ======\n\n  * a target is an e-mail,\n    telephone, or webhook\n"
            }
          ],
          'code_samples' => [],
          'media' => [],
          'exceptions' => []
        )
      )

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/list item on line 3 is physically wrapped/, error)
    end
  end

  def test_build_refuses_new_page_without_missing_source_guard
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      plan = File.join(dir, 'plan.yml')
      File.write(
        plan,
        YAML.dump(
          'schema' => 2,
          'replacements' => [],
          'new_pages' => [
            { 'language' => 'cs', 'page' => 'navody:test', 'body' => 'replacement' }
          ],
          'media' => [],
          'exceptions' => []
        )
      )

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/does not mark the page missing/, error)
    end
  end

  def test_build_rejects_malformed_capture_asset_ids
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      captures = File.join(dir, 'captures')
      write_sources(source)
      write_capture_fixture(captures)
      plan = write_media_plan(dir, '../notifications/routes')

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--captures', captures,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/invalid capture asset ID/, error)
    end
  end

  def test_build_rejects_capture_media_in_another_language_namespace
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      captures = File.join(dir, 'captures')
      write_sources(source)
      write_capture_fixture(captures)
      capture_index_path = File.join(captures, 'captures.json')
      capture_index = JSON.parse(File.read(capture_index_path))
      capture_index.fetch('assets').first.fetch('variants').fetch('cs')
                   .fetch('wiki')['media_id'] = 'en:screenshots:vpsadmin:notifications:routes.png'
      File.write(capture_index_path, JSON.dump(capture_index))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_media_plan(dir, 'notifications/routes'),
        '--captures', captures,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/media ID is in another language namespace/, error)
    end
  end

  def test_manifest_rejects_candidate_from_an_older_source_snapshot
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      build_candidate(source, write_plan(dir), candidate)

      index_path = File.join(source, 'index.json')
      index = JSON.parse(File.read(index_path))
      page = index.fetch('en').first
      content = "A concurrently updated production page.\n"
      File.binwrite(File.join(source, page.fetch('file')), content)
      page['revision'] = '124'
      page['sha256'] = Digest::SHA256.hexdigest(content)
      File.write(index_path, JSON.dump(index))

      _output, error, status = Open3.capture3(
        MANIFEST,
        '--source', source,
        '--candidate', candidate,
        '--language', 'en',
        '--summary', 'English change summary',
        '--output', File.join(dir, 'kb-release-en.yml')
      )
      refute(status.success?)
      assert_match(/candidate was built from a different source snapshot/, error)
    end
  end

  def test_review_ledger_shows_the_complete_explicit_replacement
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      write_sources(source)
      plan_path = write_plan(dir)
      plan = YAML.safe_load_file(plan_path)
      replacement = plan.fetch('replacements').first
      replacement['before'] = 'Použij Upravit profil.'
      replacement['body'] = 'Upravit profil'
      replacement['replacement'] = 'Použij <vpsadmin-nav id="member.edit-profile.open">Upravit profil</vpsadmin-nav> bezpečně.'
      plan['replacements'] = [replacement]
      File.write(plan_path, YAML.dump(plan))

      build_candidate(source, plan_path, candidate)

      review = File.read(File.join(candidate, 'review.md'))
      assert_includes(review, replacement.fetch('replacement'))
      assert_includes(
        File.read(File.join(candidate, 'cs/navody/test.txt')),
        replacement.fetch('replacement')
      )
    end
  end

  def test_build_uses_git_only_managed_article_changes
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      write_managed_pages(code_root, "<page>manuals:test</page>\nCanonical Czech.\n", "Canonical English.\n")
      run_git(code_root, 'add', 'contract/pages')
      run_git(code_root, 'commit', '-m', 'Update managed article fixture')
      head = run_git(code_root, 'rev-parse', 'HEAD').strip
      plan = write_managed_plan(dir)

      output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )
      assert(status.success?, error)
      assert_match(/2 changed pages/, output)
      assert_equal(
        "<page>manuals:test</page>\nCanonical Czech.\n",
        File.read(File.join(candidate, 'cs/navody/test.txt'))
      )
      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      assert_equal(%w[git_only git_only], index.fetch('managed_pages').map { |page| page.fetch('status') })
      provenance = index.fetch('managed_contract')
      assert_equal(base, provenance.fetch('base_commit'))
      assert_equal(head, provenance.fetch('head_commit'))
      assert_equal(2, provenance.fetch('pages').length)
      assert_equal(
        [{
          'article' => 'guide',
          'pattern' => 'kb/guide#*',
          'source' => 'tests/suite/kb/guide.nix',
          'sha256' => Digest::SHA256.hexdigest("{ ... }: { }\n")
        }],
        provenance.fetch('tests')
      )
      assert_equal(
        Digest::SHA256.file(File.join(code_root, 'contract/articles.yml')).hexdigest,
        provenance.fetch('registry_sha256')
      )

      manifest_path = File.join(dir, 'kb-release-cs.yml')
      run_manifest(source, candidate, 'cs', 'Publish managed guide', manifest_path)
      manifest_contract = YAML.safe_load_file(manifest_path).fetch('contract')
      assert_equal(base, manifest_contract.fetch('base_commit'))
      assert_equal(head, manifest_contract.fetch('head_commit'))
      assert_equal(
        [{
          'id' => 'navody:test',
          'article' => 'guide',
          'source' => 'contract/pages/cs.txt',
          'sha256' => Digest::SHA256.hexdigest("<page>manuals:test</page>\nCanonical Czech.\n")
        }],
        manifest_contract.fetch('pages')
      )
      assert_equal(provenance.fetch('tests'), manifest_contract.fetch('tests'))
    end
  end

  def test_build_rejects_a_test_source_not_derived_from_its_suite
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      contract_path = File.join(code_root, 'contract/articles.yml')
      contract = YAML.safe_load_file(contract_path)
      contract.dig('articles', 'guide', 'test')['source'] = 'tests/suite/kb/other.nix'
      File.write(contract_path, YAML.dump(contract))
      other = File.join(code_root, 'tests/suite/kb/other.nix')
      File.write(other, "{ ... }: { }\n")
      run_git(code_root, 'add', 'contract/articles.yml', 'tests/suite/kb/other.nix')
      run_git(code_root, 'commit', '-m', 'Change managed test source')

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )

      refute(status.success?)
      assert_match(/managed test source must be tests\/suite\/kb\/guide\.nix/, error)
    end
  end

  def test_manifest_rejects_a_candidate_test_source_not_derived_from_its_suite
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      write_managed_pages(code_root, "<page>manuals:test</page>\nNew Czech.\n", "New English.\n")
      run_git(code_root, 'add', 'contract/pages')
      run_git(code_root, 'commit', '-m', 'Update managed pages')
      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )
      assert(status.success?, error)
      index_path = File.join(candidate, 'index.json')
      index = JSON.parse(File.read(index_path))
      index.fetch('managed_contract').fetch('tests').first['source'] =
        'tests/suite/kb/other.nix'
      File.write(index_path, JSON.pretty_generate(index))

      _output, error, status = Open3.capture3(
        MANIFEST,
        '--source', source,
        '--candidate', candidate,
        '--language', 'cs',
        '--summary', 'Publish managed guide',
        '--output', File.join(dir, 'kb-release-cs.yml')
      )

      refute(status.success?)
      assert_match(/managed test source must be tests\/suite\/kb\/guide\.nix/, error)
    end
  end

  def test_manifest_preserves_legacy_candidate_without_test_provenance
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      write_managed_pages(code_root, "<page>manuals:test</page>\nNew Czech.\n", "New English.\n")
      run_git(code_root, 'add', 'contract/pages')
      run_git(code_root, 'commit', '-m', 'Update managed pages')
      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )
      assert(status.success?, error)
      index_path = File.join(candidate, 'index.json')
      index = JSON.parse(File.read(index_path))
      index.fetch('managed_contract').delete('tests')
      File.write(index_path, JSON.pretty_generate(index))
      manifest_path = File.join(dir, 'kb-release-cs.yml')

      run_manifest(source, candidate, 'cs', 'Publish managed guide', manifest_path)

      manifest_data = YAML.safe_load_file(manifest_path)
      refute(manifest_data.fetch('contract').key?('tests'))
      assert_instance_of(KbRelease::Manifest, KbRelease::Manifest.new(manifest_path))
    end
  end

  def test_build_rejects_a_legacy_transform_for_a_listed_managed_page
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      plan = write_managed_plan(dir)
      data = YAML.safe_load_file(plan)
      data['content_replacements'] = [{
        'language' => 'cs',
        'page' => 'navody:test',
        'before' => 'Upravit profil',
        'replacement' => 'Untracked replacement'
      }]
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--code-base', base,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/registered managed pages cannot be changed through content_replacements/, error)
    end
  end

  def test_build_rejects_a_legacy_transform_for_an_omitted_managed_page
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      plan = write_managed_plan(dir)
      data = YAML.safe_load_file(plan)
      data['managed_articles'] = []
      data['replacements'] = [{
        'language' => 'en',
        'page' => 'manuals:test',
        'path' => 'member.edit-profile.open',
        'before' => 'Edit profile'
      }]
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--code-base', base,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/registered managed pages cannot be changed through replacements/, error)
    end
  end

  def test_build_rejects_uncommitted_canonical_sources
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      write_managed_pages(code_root, "<page>manuals:test</page>\nDirty Czech.\n", "Dirty English.\n")

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', base,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/canonical source differs from contract HEAD/, error)
    end
  end

  def test_build_rejects_a_symbolic_managed_article_base
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      write_managed_code_root(code_root)

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', 'HEAD',
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/base must be a full 40-character commit OID/, error)
    end
  end

  def test_reconcile_reports_an_unchanged_managed_article
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)

      output, error, status = Open3.capture3(
        RECONCILE,
        '--source', source,
        '--code-root', code_root,
        '--article', 'guide',
        '--base', base
      )
      assert(status.success?, error)
      assert_includes(output, 'cs:navody:test in-sync')
      assert_includes(output, 'en:manuals:test in-sync')
    end
  end

  def test_build_rejects_wiki_only_managed_article_changes
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      replace_source_page(source, 'cs', 'navody:test', "<page>manuals:test</page>\nDirect wiki edit.\n")

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_managed_plan(dir),
        '--code-root', code_root,
        '--code-base', base,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/wiki-only/, error)
      assert_match(/kb-contract-reconcile/, error)
    end
  end

  def test_reconcile_explicitly_adopts_wiki_only_changes
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      direct_edit = "<page>manuals:test</page>\nDirect wiki edit.\n"
      replace_source_page(source, 'cs', 'navody:test', direct_edit)

      _output, error, status = Open3.capture3(
        RECONCILE,
        '--source', source,
        '--code-root', code_root,
        '--article', 'guide',
        '--base', base,
        '--adopt',
        '--yes'
      )
      assert(status.success?, error)
      assert_equal(direct_edit, File.read(File.join(code_root, 'contract/pages/cs.txt')))
    end
  end

  def test_reconcile_accepts_an_already_reconciled_wiki_edit
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      direct_edit = "<page>manuals:test</page>\nDirect wiki edit.\n"
      replace_source_page(source, 'cs', 'navody:test', direct_edit)
      File.write(File.join(code_root, 'contract/pages/cs.txt'), direct_edit)

      output, error, status = Open3.capture3(
        RECONCILE,
        '--source', source,
        '--code-root', code_root,
        '--article', 'guide',
        '--base', base
      )
      assert(status.success?, error)
      assert_includes(output, 'cs:navody:test reconciled')
    end
  end

  def test_reconcile_rejects_concurrent_git_and_wiki_changes
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = write_managed_code_root(code_root)
      replace_source_page(source, 'cs', 'navody:test', "<page>manuals:test</page>\nDirect wiki edit.\n")
      File.write(
        File.join(code_root, 'contract/pages/cs.txt'),
        "<page>manuals:test</page>\nConcurrent Git edit.\n"
      )

      _output, error, status = Open3.capture3(
        RECONCILE,
        '--source', source,
        '--code-root', code_root,
        '--article', 'guide',
        '--base', base
      )
      refute(status.success?)
      assert_match(/Three-way conflict/, error)
      assert_match(/merge them manually/, error)
    end
  end

  def test_build_bootstraps_a_new_managed_article_with_guarded_hashes
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      base = initialize_empty_code_root(code_root)
      write_managed_contract(code_root)
      write_managed_pages(code_root, "<page>manuals:test</page>\nCanonical Czech.\n", "Canonical English.\n")
      run_git(code_root, 'add', 'contract', 'tests')
      run_git(code_root, 'commit', '-m', 'Add managed article fixture')
      plan = write_managed_plan(dir)
      data = YAML.safe_load_file(plan)
      data.fetch('managed_articles').first['bootstrap'] = source_hashes(source)
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )
      assert(status.success?, error)
      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      assert_equal(%w[bootstrap bootstrap], index.fetch('managed_pages').map { |page| page.fetch('status') })
    end
  end

  def test_build_creates_a_missing_managed_bootstrap_page
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      candidate = File.join(dir, 'kb-candidates')
      code_root = File.join(dir, 'code')
      write_sources(source)
      mark_source_missing(source, 'en', 'manuals:test')
      base = initialize_empty_code_root(code_root)
      write_managed_contract(code_root)
      write_managed_pages(
        code_root,
        "<page>manuals:test</page>\nCanonical Czech.\n",
        "Canonical English.\n"
      )
      run_git(code_root, 'add', 'contract', 'tests')
      run_git(code_root, 'commit', '-m', 'Add managed article fixture')
      plan = write_managed_plan(dir)
      data = YAML.safe_load_file(plan)
      data.fetch('managed_articles').first['bootstrap'] = source_hashes(source)
      File.write(plan, YAML.dump(data))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan,
        '--code-root', code_root,
        '--code-base', base,
        '--output', candidate
      )
      assert(status.success?, error)
      index = JSON.parse(File.read(File.join(candidate, 'index.json')))
      english = index.fetch('pages').find do |page|
        page.values_at('language', 'id') == %w[en manuals:test]
      end
      assert_equal('create', english.fetch('policy'))
      assert_equal(
        %w[bootstrap bootstrap],
        index.fetch('managed_pages').map { |page| page.fetch('status') }
      )

      manifest_path = File.join(dir, 'kb-release-en.yml')
      run_manifest(source, candidate, 'en', 'Publish managed guide', manifest_path)
      manifest_page = YAML.safe_load_file(manifest_path).fetch('pages').first
      assert_equal('create', manifest_page.fetch('policy'))
      refute(manifest_page.key?('source_revision'))
    end
  end

  def test_build_rejects_index_paths_outside_the_source_root
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      index_path = File.join(source, 'index.json')
      index = JSON.parse(File.read(index_path))
      index.fetch('cs').first['file'] = '../outside.txt'
      File.write(index_path, JSON.dump(index))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', write_plan(dir),
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/file path escapes its root/, error)
    end
  end

  def test_build_rejects_malformed_semantic_ids
    Dir.mktmpdir do |dir|
      source = File.join(dir, 'kb-sources')
      write_sources(source)
      plan_path = write_plan(dir)
      plan = YAML.safe_load_file(plan_path)
      plan.fetch('replacements').first['path'] = '../not-semantic'
      File.write(plan_path, YAML.dump(plan))

      _output, error, status = Open3.capture3(
        BUILD,
        '--source', source,
        '--plan', plan_path,
        '--output', File.join(dir, 'kb-candidates')
      )
      refute(status.success?)
      assert_match(/invalid semantic path ID/, error)
    end
  end

  private

  def initialize_empty_code_root(root)
    FileUtils.mkdir_p(root)
    run_git(root, 'init', '--initial-branch=master')
    run_git(root, 'config', 'user.name', 'KB Test')
    run_git(root, 'config', 'user.email', 'kb-test@example.test')
    File.write(File.join(root, 'README.md'), "fixture\n")
    run_git(root, 'add', 'README.md')
    run_git(root, 'commit', '-m', 'Initialize fixture')
    run_git(root, 'rev-parse', 'HEAD').strip
  end

  def write_managed_code_root(root)
    initialize_empty_code_root(root)
    write_managed_contract(root)
    write_managed_pages(
      root,
      "<page>manuals:test</page>\nPoužij Upravit profil.\n",
      "Use Edit profile.\n"
    )
    run_git(root, 'add', 'contract', 'tests')
    run_git(root, 'commit', '-m', 'Add managed article fixture')
    run_git(root, 'rev-parse', 'HEAD').strip
  end

  def write_managed_contract(root)
    path = File.join(root, 'contract/articles.yml')
    FileUtils.mkdir_p(File.dirname(path))
    File.write(
      path,
      YAML.dump(
        'schema' => 1,
        'repository' => 'vpsfreecz/vpsfree-kb-contracts',
        'articles' => {
          'guide' => {
            'test' => {
              'suite' => 'kb/guide',
              'source' => 'tests/suite/kb/guide.nix'
            },
            'pages' => {
              'cs' => {
                'id' => 'navody:test',
                'counterpart' => 'manuals:test',
                'source' => 'contract/pages/cs.txt'
              },
              'en' => {
                'id' => 'manuals:test',
                'counterpart' => 'navody:test',
                'source' => 'contract/pages/en.txt'
              }
            }
          }
        }
      )
    )
    test_path = File.join(root, 'tests/suite/kb/guide.nix')
    FileUtils.mkdir_p(File.dirname(test_path))
    File.write(test_path, "{ ... }: { }\n")
  end

  def write_managed_pages(root, czech, english)
    pages = File.join(root, 'contract/pages')
    FileUtils.mkdir_p(pages)
    File.write(File.join(pages, 'cs.txt'), czech)
    File.write(File.join(pages, 'en.txt'), english)
  end

  def write_managed_plan(root)
    path = File.join(root, 'managed-plan.yml')
    File.write(
      path,
      YAML.dump(
        'schema' => 4,
        'replacements' => [],
        'managed_articles' => [{ 'id' => 'guide' }],
        'exceptions' => []
      )
    )
    path
  end

  def replace_source_page(root, language, page_id, content)
    index_path = File.join(root, 'index.json')
    index = JSON.parse(File.read(index_path))
    entry = index.fetch(language).find { |page| page.fetch('id') == page_id }
    File.write(File.join(root, entry.fetch('file')), content)
    entry['sha256'] = Digest::SHA256.hexdigest(content)
    entry['revision'] = entry.fetch('revision').to_i.next.to_s
    File.write(index_path, JSON.dump(index))
  end

  def source_hashes(root)
    index = JSON.parse(File.read(File.join(root, 'index.json')))
    %w[cs en].to_h do |language|
      [language, index.fetch(language).first.fetch('sha256')]
    end
  end

  def run_git(root, *args)
    output, error, status = Open3.capture3('git', '-C', root, *args)
    raise error unless status.success?

    output
  end

  def write_sources(root)
    pages = {
      'cs' => {
        'navody:test' => "<page>manuals:test</page>\nPoužij Upravit profil.\n"
      },
      'en' => {
        'manuals:test' => "Use Edit profile.\n"
      }
    }
    index = pages.to_h do |language, language_pages|
      entries = language_pages.map do |page_id, content|
        relative = File.join(language, *page_id.split(':')) + '.txt'
        destination = File.join(root, relative)
        FileUtils.mkdir_p(File.dirname(destination))
        File.binwrite(destination, content)
        {
          'id' => page_id,
          'file' => relative,
          'revision' => '123',
          'sha256' => Digest::SHA256.hexdigest(content)
        }
      end
      [language, entries]
    end
    File.write(File.join(root, 'index.json'), JSON.dump(index))
  end

  def add_source_page(root, language, page_id, content, revision:)
    index_path = File.join(root, 'index.json')
    index = JSON.parse(File.read(index_path))
    relative = File.join(language, *page_id.split(':')) + '.txt'
    destination = File.join(root, relative)
    FileUtils.mkdir_p(File.dirname(destination))
    File.binwrite(destination, content)
    index.fetch(language) << {
      'id' => page_id,
      'file' => relative,
      'revision' => revision,
      'sha256' => Digest::SHA256.hexdigest(content)
    }
    File.write(index_path, JSON.dump(index))
  end

  def write_changes(root, entries)
    path = File.join(root, 'release-changes.yml')
    File.write(path, YAML.dump('schema' => 1, 'changes' => entries))
    path
  end

  def write_plan(root)
    path = File.join(root, 'plan.yml')
    plan = {
      'schema' => 1,
      'replacements' => [
        {
          'language' => 'cs',
          'page' => 'navody:test',
          'path' => 'member.edit-profile.open',
          'before' => 'Upravit profil'
        },
        {
          'language' => 'en',
          'page' => 'manuals:test',
          'path' => 'member.edit-profile.open',
          'before' => 'Edit profile'
        }
      ],
      'exceptions' => []
    }
    File.write(path, YAML.dump(plan))
    path
  end

  def add_missing_source(root, language, page_id)
    index_path = File.join(root, 'index.json')
    index = JSON.parse(File.read(index_path))
    relative = File.join(language, *page_id.split(':')) + '.txt'
    destination = File.join(root, relative)
    FileUtils.mkdir_p(File.dirname(destination))
    File.binwrite(destination, '')
    index.fetch(language) << {
      'id' => page_id,
      'file' => relative,
      'missing' => true,
      'sha256' => Digest::SHA256.hexdigest('')
    }
    File.write(index_path, JSON.dump(index))
  end

  def mark_source_missing(root, language, page_id)
    index_path = File.join(root, 'index.json')
    index = JSON.parse(File.read(index_path))
    entry = index.fetch(language).find { |page| page.fetch('id') == page_id }
    File.binwrite(File.join(root, entry.fetch('file')), '')
    entry.delete('revision')
    entry['missing'] = true
    entry['sha256'] = Digest::SHA256.hexdigest('')
    File.write(index_path, JSON.dump(index))
  end

  def write_capture_fixture(root)
    png = "\x89PNG\r\n\x1a\nfixture".b
    assets = {
      'schema' => 5,
      'assets' => [
        {
          'id' => 'notifications/routes',
          'variants' => %w[cs en].to_h do |language|
            output = "screenshots/#{language}/notifications/routes.png"
            destination = File.join(root, output)
            FileUtils.mkdir_p(File.dirname(destination))
            File.binwrite(destination, png)
            [language, {
              'wiki' => {
                'media_id' => "#{language}:screenshots:vpsadmin:notifications:routes.png"
              },
              'output' => output,
              'sha256' => Digest::SHA256.hexdigest(png)
            }]
          end
        }
      ]
    }
    FileUtils.mkdir_p(root)
    File.write(File.join(root, 'captures.json'), JSON.dump(assets))
  end

  def write_media_plan(root, capture_id)
    path = File.join(root, 'media-plan.yml')
    File.write(
      path,
      YAML.dump(
        'schema' => 2,
        'replacements' => [],
        'new_pages' => [],
        'media' => [{ 'language' => 'cs', 'capture' => capture_id }],
        'exceptions' => []
      )
    )
    path
  end

  def run_manifest(source, candidate, language, summary, output)
    _stdout, error, status = Open3.capture3(
      MANIFEST,
      '--source', source,
      '--candidate', candidate,
      '--language', language,
      '--summary', summary,
      '--output', output
    )
    assert(status.success?, error)
  end

  def run_changes_manifest(source, candidate, language, changes, output)
    _stdout, error, status = Open3.capture3(
      MANIFEST,
      '--source', source,
      '--candidate', candidate,
      '--language', language,
      '--changes', changes,
      '--output', output
    )
    assert(status.success?, error)
  end

  def build_candidate(source, plan, candidate)
    _output, error, status = Open3.capture3(
      BUILD,
      '--source', source,
      '--plan', plan,
      '--output', candidate
    )
    assert(status.success?, error)
  end
end
