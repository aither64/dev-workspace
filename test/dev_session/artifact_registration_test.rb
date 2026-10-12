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
end
