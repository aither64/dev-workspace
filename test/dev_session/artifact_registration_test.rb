# frozen_string_literal: true

require_relative '../support/dev_session_test_case'

class DevSessionTest < Minitest::Test
  def test_artifact_registration_canonicalizes_captions_and_preserves_scope
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      path = File.join(workspace, 'work', slug, 'portal.yml')
      fixture = File.expand_path('../fixtures/portal-manifest-valid-artifact-names.yml', __dir__)
      FileUtils.cp(fixture, path)
      before = File.binread(path)
      parsed = runner.send(:load_portal_manifest, path, required: true)
      assert_equal(['Report', 'Canonical caption', 'Fallback caption', 'Unicode fallback caption'], parsed['artifacts'].map { |a| a['label'] })
      assert_equal(before, File.binread(path), 'reading must not rewrite tracking')

      runner.artifact_add(slug, './report.json', label: 'Updated report', as_is: true)
      updated = YAML.safe_load(File.read(path))
      assert_equal([], updated['repositories'])
      assert_equal(4, updated['artifacts'].length)
      assert_equal('Updated report', updated['artifacts'].first['label'])
      assert_equal('report.json', updated['artifacts'].first['path'])
      refute(updated['artifacts'].any? { |a| a.key?('name') })
      runner.artifact_add(slug, 'new.json', label: 'New report', as_is: true)
      assert_equal(5, YAML.safe_load(File.read(path))['artifacts'].length)

      stable = File.binread(path)
      ['../outside.json', '/tmp/outside.json', 'plan.md', 'state.md'].each do |bad|
        assert_raises(DevSession::Error) { runner.artifact_add(slug, bad, label: 'Bad', as_is: true) }
        assert_equal(stable, File.binread(path))
      end
      assert_raises(DevSession::Error) do
        runner.artifact_add(slug, 'too-large.json', label: 'x' * DevSession::PORTAL_MANIFEST_MAX_SIZE, as_is: true)
      end
      assert_equal(stable, File.binread(path))
      assert_raises(DevSession::Error) do
        runner.artifact_add(slug, 'blank.json', label: "\u00a0\u0085\u3000", as_is: true)
      end
      assert_equal(stable, File.binread(path))
    end
  end

  def test_artifact_registration_initializes_an_omitted_ready_artifact_list
    with_workspace do |workspace|
      slug = '2026-09-03-example'
      runner = runner_for(workspace)
      runner.ensure_tracking_files(slug)
      fixture = File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__)
      manifest = YAML.safe_load(File.read(fixture))
      manifest.delete('artifacts')
      path = File.join(workspace, 'work', slug, 'portal.yml')
      File.write(path, YAML.dump(manifest))

      runner.artifact_add(slug, 'report.json', label: 'Report', as_is: true)
      updated = YAML.safe_load(File.read(path))
      assert_equal([{ 'label' => 'Report', 'path' => 'report.json' }], updated['artifacts'])
      assert_equal(manifest, updated.reject { |key, _| key == 'artifacts' })
    end
  end

  def test_artifact_registration_refuses_reserved_or_missing_sessions
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-09-03-example'
      runner.ensure_tracking_files(slug)
      manifest = runner.send(:new_threadless_portal_manifest, slug)
      runner.send(:write_portal_manifest, slug, manifest)
      path = File.join(workspace, 'work', slug, 'portal.yml')
      before = File.binread(path)
      lock_root = File.join(workspace, 'worktrees', '.locks')
      FileUtils.mkdir_p(lock_root)
      %w[archive archive-cleanup removal revive start fork creation].each do |kind|
        journal = File.join(lock_root, "#{slug}.#{kind}.json")
        File.write(journal, '{}')
        assert_raises(DevSession::Error) { runner.artifact_add(slug, 'report.json', label: 'Report', as_is: true) }
        assert_equal(before, File.binread(path))
        File.unlink(journal)
      end
      assert_raises(DevSession::Error) { runner.artifact_add('missing', 'report.json', label: 'Report', as_is: true) }
      refute(File.exist?(File.join(workspace, 'work', 'missing')))
    end
  end

  def test_artifact_registration_cli_uses_typed_options
    output, errors = StringIO.new, StringIO.new
    calls = []
    fake = Object.new
    fake.define_singleton_method(:artifact_add) { |*args, **options| calls << [args, options] }
    cli = DevSession::CLI.new(['artifact', 'add', 'example', 'report.json', '--label', 'Report', '--as-is'], out: output, err: errors)
    cli.define_singleton_method(:runner) { fake }
    assert_equal(0, cli.run, errors.string)
    assert_equal([[['example', 'report.json'], { label: 'Report', as_is: true }]], calls)
  end

  def test_artifact_registration_preserves_a_concurrent_manifest_edit
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-09-03-example'
      runner.ensure_tracking_files(slug)
      runner.send(:write_portal_manifest, slug, runner.send(:new_threadless_portal_manifest, slug))
      path = File.join(workspace, 'work', slug, 'portal.yml')
      concurrent = File.binread(path) + "# concurrent edit\n"
      writer = runner.method(:write_portal_manifest)
      runner.define_singleton_method(:write_portal_manifest) do |*args, **options|
        File.binwrite(path, concurrent)
        writer.call(*args, **options)
      end
      error = assert_raises(DevSession::Error) do
        runner.artifact_add(slug, 'report.json', label: 'Report', as_is: true)
      end
      assert_match(/changed during artifact registration/, error.message)
      assert_equal(concurrent, File.binread(path))
      assert_empty(Dir[File.join(File.dirname(path), '.portal.yml.*.tmp')])
    end
  end
  def test_artifact_registration_accepts_completed_creation_receipts
    [1, 2, 3, :preserved].each do |schema|
      with_artifact_creation_receipt(schema) do |runner, slug, manifest_path, journal_path|
        before = YAML.safe_load(File.read(manifest_path))
        receipt = File.binread(journal_path)
        runner.artifact_add(slug, 'new.json', label: 'New report', as_is: true)
        runner.artifact_add(slug, 'new.json', label: 'Updated report', as_is: true)
        after = YAML.safe_load(File.read(manifest_path))
        assert_equal(before.reject { |key, _| key == 'artifacts' },
                     after.reject { |key, _| key == 'artifacts' })
        assert_equal(before['artifacts'] + [{ 'label' => 'Updated report', 'path' => 'new.json' }], after['artifacts'])
        assert_equal(receipt, File.binread(journal_path), 'registration must preserve the completion receipt')
      end
    end
  end

  def test_artifact_registration_rejects_invalid_or_incomplete_creation_receipts
    changes = [
      ->(journal, _manifest) { journal['state'] = 'creating'; journal['tmux_identity'] = 'c' * 64 },
      ->(journal, _manifest) { journal['slug'] = '2026-09-03-other' },
      ->(journal, _manifest) { journal['goal_sha256'] = 'b' * 64 },
      ->(journal, _manifest) { journal['schema'] = 99 },
      ->(journal, _manifest) { journal['unexpected'] = true },
      ->(_journal, manifest) { manifest['creation']['state'] = 'creating' },
      ->(_journal, manifest) { manifest['creation']['initial_goal_sent'] = false },
      ->(_journal, manifest) { manifest.delete('creation') }
    ]
    changes.each do |change|
      with_artifact_creation_receipt(1) do |runner, slug, manifest_path, journal_path|
        manifest = YAML.safe_load(File.read(manifest_path))
        journal = JSON.parse(File.read(journal_path))
        change.call(journal, manifest)
        File.write(manifest_path, YAML.dump(manifest))
        File.write(journal_path, JSON.generate(journal))
        before = File.binread(manifest_path)
        receipt = File.binread(journal_path)
        assert_raises(DevSession::Error) do
          runner.artifact_add(slug, 'new.json', label: 'New report', as_is: true)
        end
        assert_equal(before, File.binread(manifest_path))
        assert_equal(receipt, File.binread(journal_path))
      end
    end
  end

  def test_artifact_registration_requires_a_private_bounded_json_receipt
    ['not json', '{"schema":1,"schema":1}', 'x' * (64 * 1024 + 1)].each do |payload|
      with_artifact_creation_receipt(1) do |runner, slug, manifest_path, journal_path|
        File.write(journal_path, payload)
        before = File.binread(manifest_path)
        assert_raises(DevSession::Error) { runner.artifact_add(slug, 'new.json', label: 'New report', as_is: true) }
        assert_equal(before, File.binread(manifest_path))
        assert_equal(payload, File.binread(journal_path))
      end
    end
    with_artifact_creation_receipt(1) do |runner, slug, manifest_path, journal_path|
      File.chmod(0o644, journal_path)
      before = File.binread(manifest_path)
      assert_raises(DevSession::Error) { runner.artifact_add(slug, 'new.json', label: 'New report', as_is: true) }
      assert_equal(before, File.binread(manifest_path))
    end
  end

  private

  def with_artifact_creation_receipt(schema)
    with_workspace do |workspace|
      runner = runner_for(workspace)
      slug = '2026-09-03-example'
      runner.ensure_tracking_files(slug)
      manifest_path = File.join(workspace, 'work', slug, 'portal.yml')
      fixture = File.expand_path('../fixtures/portal-manifest-valid.yml', __dir__)
      FileUtils.cp(fixture, manifest_path)
      manifest = YAML.safe_load(File.read(manifest_path))
      journal = {
        'schema' => schema == :preserved ? 1 : schema, 'slug' => slug,
        'goal_sha256' => manifest.dig('creation', 'goal_sha256'),
        'run_codex' => true, 'state' => 'ready'
      }
      if [2, 3].include?(schema)
        journal.merge!('model' => 'model-lead', 'effort' => 'high')
      end
      if schema == 2
        journal.merge!('agent_team_binding' => "v1.eyJ4IjoieSJ9.#{'b' * 64}",
                       'agent_team_binding_digest' => 'b' * 64)
      elsif schema == 3
        journal['direct_team'] = {
          'id' => 'solo', 'name' => 'Solo', 'description' => 'One lead thread.',
          'catalogDigest' => 'a' * 64, 'teamDigest' => 'b' * 64,
          'leadModel' => 'model-lead', 'leadEffort' => 'high',
          'roles' => ['lead'], 'members' => []
        }
      elsif schema == :preserved
        journal.merge!('preserve_tracking' => true, 'tracking_origin' => 'retained',
                       'tracking_plan_sha256' => 'c' * 64, 'tracking_state_sha256' => 'd' * 64)
      end
      journal_path = runner.send(:creation_journal_file, slug)
      File.write(journal_path, JSON.generate(journal))
      File.chmod(0o600, journal_path)
      yield runner, slug, manifest_path, journal_path
    end
  end

end
