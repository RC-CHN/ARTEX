package agent

import (
	"reflect"
	"testing"
)

func TestColdStructureMatchesPayloadGraph(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("structure fixture", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state) SELECT $1,'intent',jsonb_build_object('summary','fixture','detail',repeat('x',16384)),CASE WHEN i%3=0 THEN 'running' ELSE 'done' END FROM generate_series(1,400)i`, exp); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,dst_id,rel)
SELECT $1,a.id,b.id,'derived_from' FROM exploration_nodes a JOIN exploration_nodes b ON a.exploration_id=b.exploration_id AND a.id<b.id WHERE a.exploration_id=$1 LIMIT 6001`, exp); err != nil {
		t.Fatal(err)
	}
	full, _, err := loadColdGraph(s)
	if err != nil {
		t.Fatal(err)
	}
	light, edges, err := loadColdStructure(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 6001 {
		t.Fatal("structure silently capped edges", len(edges))
	}
	if !reflect.DeepEqual(full, light) || !reflect.DeepEqual(full.hotSet(), light.hotSet()) {
		t.Fatal("light graph changed reachability")
	}
	nodes, err := s.StructureNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if len(n.Payload) != 0 {
			t.Fatal("structure query loaded evidence payload")
		}
	}
}
