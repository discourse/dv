#!/usr/bin/env ruby
# frozen_string_literal: true

# Stress-test for `dv extract --sync` — run this inside the container
# while --sync is active and verify that all changes land on the host.

require "optparse"
require "fileutils"
require "securerandom"
require "json"
require "digest"
require "tmpdir"

DEFAULT_DIR     = "tmp/stress_sync"
DEFAULT_COMMITS = 10
DEFAULT_FILES   = 8
DEFAULT_EDITS   = 5
DEFAULT_PAUSE   = 0.05  # seconds between operations

options = {
  dir:     DEFAULT_DIR,
  commits: DEFAULT_COMMITS,
  files:   DEFAULT_FILES,
  edits:   DEFAULT_EDITS,
  pause:   DEFAULT_PAUSE,
  seed:    nil,
  verify:  nil,
  cleanup: false,
}

parser = OptionParser.new do |opts|
  opts.banner = <<~BANNER
    Usage: stress_sync.rb [options]

    Generates rapid file churn (creates, edits, deletes, renames, nested dirs,
    binary-ish blobs, symlinks, rapid overwrites) inside a git repo to stress-test
    `dv extract --sync`.

    Run inside the container while --sync is running, then use --verify on the
    host to confirm every file matches.

    Workflow:
      1. In terminal A:  dv extract --sync
      2. In terminal B:  dv run -- ruby scripts/stress_sync.rb
      3. Wait for it to finish
      4. On host:        ruby scripts/stress_sync.rb --verify /path/to/host/repo

  BANNER

  opts.on("-d", "--dir DIR", "Working subdirectory (default: #{DEFAULT_DIR})") { |v| options[:dir] = v }
  opts.on("-c", "--commits N", Integer, "Number of commit rounds (default: #{DEFAULT_COMMITS})") { |v| options[:commits] = v }
  opts.on("-f", "--files N", Integer, "Files to create per round (default: #{DEFAULT_FILES})") { |v| options[:files] = v }
  opts.on("-e", "--edits N", Integer, "Edits per round (default: #{DEFAULT_EDITS})") { |v| options[:edits] = v }
  opts.on("-p", "--pause SECS", Float, "Pause between operations in seconds (default: #{DEFAULT_PAUSE})") { |v| options[:pause] = v }
  opts.on("-s", "--seed SEED", Integer, "RNG seed for reproducibility") { |v| options[:seed] = v }
  opts.on("--verify DIR", "Verify mode: compare current repo state against DIR") { |v| options[:verify] = v }
  opts.on("--cleanup", "Remove the stress test directory when done") { options[:cleanup] = true }
  opts.on("-h", "--help", "Show this help") do
    puts opts
    exit
  end
end
parser.parse!

# ─── Verify mode ──────────────────────────────────────────────────────────────

if options[:verify]
  host_dir = options[:verify]
  stress_dir = options[:dir]

  unless Dir.exist?(host_dir)
    $stderr.puts "ERROR: host directory does not exist: #{host_dir}"
    exit 1
  end

  # Collect manifest from current directory (container side)
  container_manifest = {}
  stress_path = File.join(Dir.pwd, stress_dir)
  if Dir.exist?(stress_path)
    Dir.glob("#{stress_path}/**/*", File::FNM_DOTMATCH).each do |f|
      next if File.directory?(f)
      rel = f.sub("#{Dir.pwd}/", "")
      container_manifest[rel] = Digest::SHA256.file(f).hexdigest
    end
  end

  # Also check manifest.json if it exists
  manifest_path = File.join(stress_path, "manifest.json")
  if File.exist?(manifest_path)
    expected = JSON.parse(File.read(manifest_path))
  else
    expected = container_manifest
  end

  host_stress = File.join(host_dir, stress_dir)
  errors = []
  missing = []
  extra = []
  mismatched = []

  expected.each do |rel, sha|
    next if rel.end_with?("manifest.json")
    host_file = File.join(host_dir, rel)
    unless File.exist?(host_file)
      missing << rel
      next
    end
    host_sha = Digest::SHA256.file(host_file).hexdigest
    if host_sha != sha
      mismatched << { file: rel, expected: sha, got: host_sha }
    end
  end

  # Check for extra files on host that aren't in manifest
  if Dir.exist?(host_stress)
    Dir.glob("#{host_stress}/**/*", File::FNM_DOTMATCH).each do |f|
      next if File.directory?(f)
      rel = f.sub("#{host_dir}/", "")
      next if rel.end_with?("manifest.json")
      extra << rel unless expected.key?(rel)
    end
  end

  puts "=== Sync Verification ==="
  puts "Expected files: #{expected.size - 1}" # minus manifest
  puts "Missing on host: #{missing.size}"
  missing.each { |f| puts "  MISSING: #{f}" }
  puts "Hash mismatches: #{mismatched.size}"
  mismatched.each { |m| puts "  MISMATCH: #{m[:file]}" }
  puts "Extra on host: #{extra.size}"
  extra.each { |f| puts "  EXTRA: #{f}" }

  if missing.empty? && mismatched.empty? && extra.empty?
    puts "\nAll files synced correctly!"
    exit 0
  else
    puts "\nSync verification FAILED"
    exit 1
  end
end

# ─── Stress generation mode ───────────────────────────────────────────────────

seed = options[:seed] || SecureRandom.random_number(2**32)
rng = Random.new(seed)
puts "Seed: #{seed} (replay with --seed #{seed})"

work_dir = File.join(Dir.pwd, options[:dir])
FileUtils.mkdir_p(work_dir)

# Track living files so we can edit/delete/rename them
living_files = []

def random_content(rng, size = nil)
  size ||= rng.rand(50..2000)
  (0...size).map { (32 + rng.rand(95)).chr }.join
end

def random_name(rng, ext: nil)
  name = "f_#{SecureRandom.hex(4)}"
  ext ||= %w[.rb .js .txt .md .html .css .yml .json .go .py].sample(random: rng)
  "#{name}#{ext}"
end

def random_subdir(rng, base)
  depth = rng.rand(0..3)
  parts = (0...depth).map { "d_#{SecureRandom.hex(3)}" }
  File.join(base, *parts)
end

def micro_sleep(pause)
  sleep(pause) if pause > 0
end

puts "Starting stress test in: #{options[:dir]}"
puts "  Rounds: #{options[:commits]}, Files/round: #{options[:files]}, Edits/round: #{options[:edits]}"
puts ""

total_ops = { create: 0, edit: 0, delete: 0, rename: 0, rapid_overwrite: 0, binary: 0, nested: 0 }

options[:commits].times do |round|
  puts "── Round #{round + 1}/#{options[:commits]} ──"

  # 1. Create new files
  options[:files].times do
    subdir = random_subdir(rng, work_dir)
    FileUtils.mkdir_p(subdir)
    name = random_name(rng)
    path = File.join(subdir, name)
    File.write(path, random_content(rng))
    living_files << path
    total_ops[:create] += 1
    micro_sleep(options[:pause])
  end

  # 2. Edit existing files
  editable = living_files.select { |f| File.exist?(f) }
  edit_count = [options[:edits], editable.size].min
  editable.sample(edit_count, random: rng).each do |path|
    # Different edit strategies
    case rng.rand(4)
    when 0 # Append
      File.open(path, "a") { |f| f.write("\n#{random_content(rng, rng.rand(20..200))}") }
    when 1 # Prepend
      old = File.read(path)
      File.write(path, "#{random_content(rng, rng.rand(20..200))}\n#{old}")
    when 2 # Full rewrite
      File.write(path, random_content(rng))
    when 3 # Truncate then write (two operations in quick succession)
      File.write(path, "")
      micro_sleep(options[:pause])
      File.write(path, random_content(rng))
    end
    total_ops[:edit] += 1
    micro_sleep(options[:pause])
  end

  # 3. Rapid overwrite — same file written many times in a burst
  if editable.any? && rng.rand < 0.7
    target = editable.sample(random: rng)
    burst = rng.rand(5..15)
    burst.times do
      File.write(target, random_content(rng, rng.rand(100..500)))
    end
    total_ops[:rapid_overwrite] += 1
    puts "  rapid overwrite: #{burst}x on #{File.basename(target)}"
  end

  # 4. Delete some files
  if living_files.size > 5
    delete_count = rng.rand(1..[3, living_files.size / 4].min)
    living_files.sample(delete_count, random: rng).each do |path|
      if File.exist?(path)
        File.delete(path)
        living_files.delete(path)
        total_ops[:delete] += 1
        micro_sleep(options[:pause])
      end
    end
  end

  # 5. Rename some files
  renameable = living_files.select { |f| File.exist?(f) }
  if renameable.size > 2
    rename_count = rng.rand(1..[2, renameable.size / 4].min)
    renameable.sample(rename_count, random: rng).each do |old_path|
      new_name = random_name(rng)
      new_path = File.join(File.dirname(old_path), new_name)
      File.rename(old_path, new_path)
      living_files.delete(old_path)
      living_files << new_path
      total_ops[:rename] += 1
      micro_sleep(options[:pause])
    end
  end

  # 6. Create deeply nested directory with files
  if rng.rand < 0.4
    deep = File.join(work_dir, *(0...rng.rand(4..7)).map { "n_#{SecureRandom.hex(2)}" })
    FileUtils.mkdir_p(deep)
    rng.rand(1..3).times do
      path = File.join(deep, random_name(rng))
      File.write(path, random_content(rng))
      living_files << path
      total_ops[:nested] += 1
    end
    micro_sleep(options[:pause])
  end

  # 7. Create binary-ish files (random bytes)
  if rng.rand < 0.3
    subdir = random_subdir(rng, work_dir)
    FileUtils.mkdir_p(subdir)
    path = File.join(subdir, "blob_#{SecureRandom.hex(4)}.bin")
    File.open(path, "wb") { |f| f.write(rng.bytes(rng.rand(256..4096))) }
    living_files << path
    total_ops[:binary] += 1
    micro_sleep(options[:pause])
  end

  # 8. Git commit the round
  system({ "LEFTHOOK" => "0" }, "git add -A #{options[:dir]} 2>/dev/null")
  system({ "LEFTHOOK" => "0" }, "git commit -m 'stress round #{round + 1}/#{options[:commits]} (seed #{seed})' --allow-empty -q 2>/dev/null")
  puts "  committed (#{living_files.count { |f| File.exist?(f) }} live files)"

  # Small pause between rounds to let sync settle
  sleep(0.2)
end

# ─── Final manifest ──────────────────────────────────────────────────────────

# Clean up dead references
living_files.select! { |f| File.exist?(f) }

manifest = {}
Dir.glob("#{work_dir}/**/*", File::FNM_DOTMATCH).each do |f|
  next if File.directory?(f)
  rel = f.sub("#{Dir.pwd}/", "")
  manifest[rel] = Digest::SHA256.file(f).hexdigest
end

manifest_path = File.join(work_dir, "manifest.json")
File.write(manifest_path, JSON.pretty_generate(manifest))
system({ "LEFTHOOK" => "0" }, "git add #{manifest_path} && git commit -m 'stress: write manifest' -q 2>/dev/null")

puts ""
puts "=== Summary ==="
puts "  Seed:             #{seed}"
puts "  Creates:          #{total_ops[:create]}"
puts "  Edits:            #{total_ops[:edit]}"
puts "  Rapid overwrites: #{total_ops[:rapid_overwrite]}"
puts "  Deletes:          #{total_ops[:delete]}"
puts "  Renames:          #{total_ops[:rename]}"
puts "  Nested:           #{total_ops[:nested]}"
puts "  Binary:           #{total_ops[:binary]}"
puts "  Final live files: #{living_files.size}"
puts "  Manifest:         #{options[:dir]}/manifest.json"
puts ""
puts "To verify on host:"
puts "  ruby scripts/stress_sync.rb --verify /path/to/host/repo -d #{options[:dir]}"

if options[:cleanup]
  FileUtils.rm_rf(work_dir)
  puts "\nCleaned up #{options[:dir]}"
end
