# frozen_string_literal: true

require_relative 'workspace-coordination'

module WorkspaceCoordination
  class Certification
    NON_BEHAVIOR_PATHS = %r{\A(?:work/|archive/|notes/)}
    MIGRATIONS = %r{(?:\A|/)(?:migrations?|db/migrate)(?:/|\z)}i

    def initialize(engine)
      @engine = engine
    end

    def git(path, *args)
      @engine.capture('git', '-C', path, *args).strip
    end

    def prove(path, from_base:, from:, to_base:, to:)
      [from_base, from, to_base, to].each do |revision|
        raise Error, 'certification requires exact commits' unless revision.is_a?(String) && revision.match?(SHA)
        git(path, 'cat-file', '-e', "#{revision}^{commit}")
      end
      unless @engine.ancestor?(path, from_base, from) && @engine.ancestor?(path, to_base, to) && @engine.ancestor?(path, from_base, to_base)
        raise Error.new('certification ranges have incompatible ancestry', 3)
      end
      ["#{from_base}..#{from}", "#{to_base}..#{to}"].each do |range|
        raise Error.new('merge commit transformation needs independent review', 4) unless git(path, 'rev-list', '--min-parents=2', range).empty?
      end
      changed = git(path, 'diff', '--name-only', from_base, from).lines.map(&:strip) |
        git(path, 'diff', '--name-only', to_base, to).lines.map(&:strip)
      raise Error.new('migration changes require independent review', 4) if changed.any? { |name| name.match?(MIGRATIONS) }
      old_tree, new_tree = [from, to].map { |head| git(path, 'rev-parse', "#{head}^{tree}") }
      incoming = git(path, 'diff', '--name-only', from_base, to_base).lines.map(&:strip)
      unless old_tree == new_tree || incoming.all? { |name| name.match?(NON_BEHAVIOR_PATHS) }
        raise Error.new('substantive upstream changes need applicable review and verification', 4)
      end
      if old_tree == new_tree
        relation, reconstructed = 'identical_tree', new_tree
      else
        reconstructed = git(path, 'merge-tree', '--write-tree', "--merge-base=#{from_base}", to_base, from).lines.first
        unless reconstructed == new_tree
          raise Error.new('refreshed tree differs from the reconstructed reviewed change', 4)
        end
        relation = 'reconstructed_patch'
      end
      {'schema' => 1, 'relation' => relation, 'fromBase' => from_base, 'from' => from,
        'toBase' => to_base, 'to' => to, 'tree' => new_tree, 'reconstructedTree' => reconstructed,
        'changedPaths' => changed, 'incomingPaths' => incoming, 'migrations' => []}
    end

    # Exact check identity, including dependencies, tools and configured inputs.
    # A tree/patch certificate alone never certifies a previous test result.
    def self.reusable_check?(receipt, derivation:, inputs:)
      receipt.is_a?(Hash) && receipt['schema'] == 1 && receipt['exitStatus'] == 0 &&
        receipt['derivation'] == derivation && receipt['inputs'] == inputs
    end

    def validate_receipt(owner, mapping)
      receipt = WorkspaceCoordination.parse(File.binread(@engine.lineage_evidence(owner, mapping.fetch('evidence')), MAX_REQUEST + 1))
      WorkspaceCoordination.keys!(receipt, %w[schema ownerId reservationId mapping originalReview proof verification], %w[priorProof])
      unless receipt['schema'] == 1 && receipt['ownerId'] == owner['id'] &&
             receipt['mapping'] == mapping.reject { |key, _| key == 'evidence' }
        raise Error.new('equivalence receipt binds another owner or range', 4)
      end
      review = WorkspaceCoordination.parse(File.binread(@engine.lineage_evidence(owner, receipt.fetch('originalReview')), MAX_REQUEST + 1))
      unless review['schema'] == 1 && review['decision'] == 'accepted' && review['reviewer'].is_a?(String) && !review['reviewer'].empty? &&
             review['repository'] == mapping['repository'] && review['target'] == mapping['target'] && review['migrations'] == []
        raise Error.new('original independent review does not cover the certified range', 4)
      end
      @engine.lineage_evidence(owner, receipt.fetch('verification'))
      path, = @engine.repository(owner, mapping.fetch('repository'))
      if review['head'] != mapping['from'] || review['base'] != mapping['fromBase']
        prior = receipt['priorProof'] || raise(Error.new('intermediate range has no certified original review lineage', 4))
        bridge = {'fromBase' => review['base'], 'from' => review['head'], 'toBase' => mapping['fromBase'], 'to' => mapping['from']}
        raise Error.new('intermediate review lineage differs', 4) unless verify_proof(path, prior, bridge) == prior
      end
      actual = verify_proof(path, receipt['proof'], mapping)
      raise Error.new('equivalence proof differs from retained commits', 4) unless actual == receipt['proof']
      receipt
    end

    def verify_proof(path, proof, mapping)
      unless proof['relation'] == 'generated_pin'
        return prove(path, from_base: mapping['fromBase'], from: mapping['from'],
          to_base: mapping['toBase'], to: mapping['to'])
      end
      recipe = IntegrationCatalog.new(@engine.package).recipe(proof.fetch('recipe'))
      payload = {'schema' => 1, 'action' => 'verify', 'source' => path, 'from' => mapping['from'], 'to' => mapping['to'],
        'parameters' => proof.fetch('parameters'), 'heads' => proof.fetch('heads')}
      result = WorkspaceCoordination.parse(@engine.capture(recipe.fetch('command'), input: JSON.generate(payload)))
      raise Error.new('generated pin verification failed', 4) unless result['schema'] == 1 && result['verified'] == true && result['head'] == mapping['to']
      result.fetch('proof').merge('recipe' => proof['recipe'])
    end
  end
end
