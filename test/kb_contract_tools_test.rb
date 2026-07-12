# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'minitest/autorun'
require 'open3'
require 'tmpdir'
require 'yaml'

class KbContractToolsTest < Minitest::Test
  ROOT = File.expand_path('..', __dir__)
  BUILD = File.join(ROOT, 'bin/kb-contract-build')
  FETCH = File.join(ROOT, 'bin/kb-contract-fetch')
  MANIFEST = File.join(ROOT, 'bin/kb-contract-manifest')

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

  private

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
end
