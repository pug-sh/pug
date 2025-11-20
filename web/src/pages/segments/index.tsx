import { useState, useEffect } from 'react'
import { Plus, Search, Filter, Edit, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { segmentsService } from '@/lib/rpc'
import { useAtom } from 'jotai'
import { getActiveProjectAtom } from '@/atoms/projects'
import { Segment } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { Link } from 'wouter'

export default function Segments() {
  const [activeProject] = useAtom(getActiveProjectAtom)
  const [segments, setSegments] = useState<Segment[]>([])
  const [loading, setLoading] = useState(true)
  const [searchTerm, setSearchTerm] = useState('')

  useEffect(() => {
    if (activeProject?.id) {
      fetchSegments()
    }
  }, [activeProject?.id])

  const fetchSegments = async () => {
    try {
      setLoading(true)
      const response = await segmentsService.listSegments({
        projectId: activeProject?.id || '',
        limit: 50,
        offset: 0
      })
      setSegments(response.segments)
    } catch (error) {
      console.error('Error fetching segments:', error)
    } finally {
      setLoading(false)
    }
  }

  const handleDelete = async (segmentId: string) => {
    if (window.confirm('Are you sure you want to delete this segment?')) {
      try {
        await segmentsService.deleteSegment({ segmentId })
        fetchSegments()
      } catch (error) {
        console.error('Error deleting segment:', error)
      }
    }
  }

  const filteredSegments = segments.filter(
    (segment) =>
      segment.name.toLowerCase().includes(searchTerm.toLowerCase()) ||
      segment.description.toLowerCase().includes(searchTerm.toLowerCase())
  )

  return (
    <div className="container mx-auto py-10">
      <div className="mb-8">
        <h1 className="text-3xl font-bold tracking-tight">User Segments</h1>
        <p className="text-muted-foreground">
          Define and manage user segments based on user metadata and behavior
        </p>
      </div>

      <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4 mb-6">
        <div className="relative w-full sm:w-auto">
          <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            placeholder="Search segments..."
            className="pl-8 w-full sm:w-64"
            value={searchTerm}
            onChange={(e) => setSearchTerm(e.target.value)}
          />
        </div>
        
        <Link to="/segments/new">
          <Button>
            <Plus className="mr-2 h-4 w-4" />
            Create Segment
          </Button>
        </Link>
      </div>

      {loading ? (
        <div className="flex justify-center items-center h-64">
          <div className="animate-spin rounded-full h-10 w-10 border-b-2 border-gray-900"></div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {filteredSegments.map((segment) => (
            <Card key={segment.id} className="overflow-hidden">
              <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                <CardTitle className="text-2xl font-bold truncate">
                  {segment.name}
                </CardTitle>
                <div className="flex space-x-2">
                  <Link to={`/segments/${segment.id}/edit`}>
                    <Button variant="ghost" size="icon">
                      <Edit className="h-4 w-4" />
                    </Button>
                  </Link>
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => handleDelete(segment.id)}
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </div>
              </CardHeader>
              <CardContent>
                <p className="text-sm text-muted-foreground mb-4">
                  {segment.description || 'No description provided'}
                </p>
                
                <div className="flex items-center text-sm text-muted-foreground">
                  <Filter className="mr-2 h-4 w-4" />
                  <span>Conditions: {segment.filter?.conditions?.length || 0}</span>
                </div>
                
                <div className="mt-4 pt-4 border-t">
                  <div className="flex justify-between text-sm">
                    <span className="text-muted-foreground">Status</span>
                    <span className={`px-2 py-1 rounded-full text-xs ${
                      segment.isActive 
                        ? 'bg-green-100 text-green-800' 
                        : 'bg-red-100 text-red-800'
                    }`}>
                      {segment.isActive ? 'Active' : 'Inactive'}
                    </span>
                  </div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {filteredSegments.length === 0 && !loading && (
        <div className="text-center py-12">
          <div className="mx-auto h-24 w-24 rounded-full bg-gray-100 flex items-center justify-center mb-4">
            <Filter className="h-12 w-12 text-gray-400" />
          </div>
          <h3 className="text-lg font-medium mb-1">No segments yet</h3>
          <p className="text-muted-foreground mb-4">
            Get started by creating your first user segment
          </p>
          <Link to="/segments/new">
            <Button>
              <Plus className="mr-2 h-4 w-4" />
              Create Segment
            </Button>
          </Link>
        </div>
      )}
    </div>
  )
}